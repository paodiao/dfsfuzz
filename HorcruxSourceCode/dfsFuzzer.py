# -*- coding: utf-8 -*-

# repeat a file operation N times
# allow for multi-thread tests with stonewalling
# we can launch any combination of these to simulate more complex workloads
# possible enhancements:
#    embed parallel python and thread launching logic so we can have both
#    CLI and GUI interfaces to same code
#
# to run all unit tests:
#   python smallfile.py
# to run just one of unit tests do
#   python -m unittest smallfile.Test.your-unit-test
# alternative single-test syntax:
#   python smallfile.py -v Test.test_c1_Mkdir
#
# on older Fedoras:
#   yum install python-unittest2
# on Fedora 33 with python 3.9.2, unittest is built in and no package is needed


import codecs
import copy
import errno
import logging
import math
import os
import signal
import os.path
import random
import socket
import sys
import threading
import time
import multiprocessing
import string
from os.path import exists, join
from shutil import rmtree
from oracle import *
from fileTree import *
import stat
import commands
from commands_map import *

from sync_files import ensure_deleted, ensure_dir_exists, touch

OK = 0  # system call return code for success
NOTOK = 1
KB_PER_GB = 1 << 20
USEC_PER_SEC = 1000000.0

# min % of files processed considered acceptable for a test run
# this should be a parameter but we'll just lower it to 70% for now
# FIXME: should be able to calculate default based on thread count, etc.
pct_files_min = 70

# we have to support a variety of python environments,
# so for optional features don't blow up if they aren't there, just remember

xattr_installed = False
try:
    import xattr

    xattr_installed = True
except ImportError:
    pass

fadvise_installed = False
try:
    import drop_buffer_cache

    fadvise_installed = True
except ImportError:
    pass

fallocate_installed = False
try:
    import fallocate  # not yet in python os module

    fallocate_installed = True
except ImportError:
    pass

unittest_module = None
try:
    import unittest2

    unittest_module = unittest2
except ImportError:
    pass

try:
    import unittest

    unittest_module = unittest
except ImportError:
    pass

# makes using python -m pdb easier with unit tests
# set .pdbrc file to contain something like:
#   b run_unit_tests
#   c
#   b Test.test_whatever


def run_unit_tests():
    if unittest_module:
        unittest_module.main()
    else:
        raise SMFRunException("no python unittest module available")


# python threading module method name isAlive changed to is_alive in python3


use_isAlive = sys.version_info[0] < 3

# Windows 2008 server seemed to have this environment variable
# didn't check if it's universal

is_windows_os = os.getenv("HOMEDRIVE") is not None

# O_BINARY variable means we don't need to special-case windows
# in every open statement

O_BINARY = 0
if is_windows_os:
    O_BINARY = os.O_BINARY

# for timeout debugging

debug_timeout = os.getenv("DEBUG_TIMEOUT")

# FIXME: pass in file pathname instead of file number


class MFRdWrExc(Exception):
    def __init__(self, opname_in, filenum_in, rqnum_in, bytesrtnd_in):
        self.opname = opname_in
        self.filenum = filenum_in
        self.rqnum = rqnum_in
        self.bytesrtnd = bytesrtnd_in

    def __str__(self):
        return "file {filenum} request {rqnum} byte count {bc} {op}".format(
            filenum=str(self.filenum),
            rqnum=str(self.rqnum),
            bc=str(self.bytesrtnd),
            op=self.opname,
        )


class SMFResultException(Exception):
    pass


class SMFRunException(Exception):
    pass


def myassert(bool_expr):
    if not bool_expr:
        raise SMFRunException("assertion failed!")


# abort routine just cleans up threads


def abort_test(abort_fn, thread_list):
    if not os.path.exists(abort_fn):
        touch(abort_fn)
    for t in thread_list:
        t.terminate()


# hide difference between python2 and python3
# python threading module method name isAlive changed to is_alive in python3


def thrd_is_alive(thrd):
    use_isAlive = sys.version_info[0] < 3
    return thrd.isAlive() if use_isAlive else thrd.is_alive()


# next two routines are for asynchronous replication
# we remember the time when a file was completely written
# and its size using xattr,
# then we read xattr in do_await_create operation
# and compute latencies from that


def remember_ctime_size_xattr(filedesc):
    nowtime = str(time.time())
    st = os.fstat(filedesc)
    xattr.setxattr(
        filedesc,
        "user.smallfile-ctime-size",
        nowtime + "," + str(st.st_size / Eris.BYTES_PER_KB),
    )


def recall_ctime_size_xattr(pathname):
    (ctime, size_kb) = (None, None)
    try:
        with open(pathname, "r") as fd:
            xattr_str = xattr.getxattr(fd, "user.smallfile-ctime-size")
            token_pair = str(xattr_str).split(",")
            ctime = float(token_pair[0][2:])
            size_kb = int(token_pair[1].split(".")[0])
    except IOError as e:
        eno = e.errno
        if eno != errno.ENODATA:
            raise e
    return (ctime, size_kb)


def get_hostname(h):
    if h is None:
        h = socket.gethostname()
    return h


def hostaddr(h):  # return the IP address of a hostname
    if h is None:
        a = socket.gethostbyname(socket.gethostname())
    else:
        a = socket.gethostbyname(h)
    return a


def hexdump(b):
    s = ""
    for j in range(0, len(b)):
        s += "%02x" % b[j]
    return s


def binary_buf_str(b):  # display a binary buffer as a text string
    if sys.version < "3":
        return codecs.unicode_escape_decode(b)[0]
    else:
        if isinstance(b, str):
            return bytes(b).decode("UTF-8", "backslashreplace")
        else:
            return b.decode("UTF-8", "backslashreplace")


class Eris:
    rename_suffix = ".rnm"
    all_op_names = [
        "create",
        "delete",
        "append",
        "overwrite",
        "read",
        "readdir",
        "rename",
        "delete-renamed",
        "cleanup",
        "symlink",
        "hardlink",
        "mkdir",
        "rmdir",
        "stat",
        "chmod",
        "setxattr",
        "getxattr",
        "swift-get",
        "swift-put",
        "ls-l",
        "await-create",
        "truncate-overwrite",
    ]
    OK = 0
    NOTOK = 1
    BYTES_PER_KB = 1024
    MICROSEC_PER_SEC = 1000000.0

    # number of files between stonewalling check at smallest file size
    max_files_between_checks = 100

    # default for UNIX
    tmp_dir = os.getenv("TMPDIR")
    if tmp_dir is None:  # windows case
        tmp_dir = os.getenv("TEMP")
    if tmp_dir is None:  # assume POSIX-like
        tmp_dir = "/var/tmp"

    # Change here to generate workloads in different directories
    tmp_dir = ""
    # constant file size
    fsdistr_fixed = -1
    # a file size distribution type that results in a few files much larger
    # than the mean and mostly files much smaller than the mean
    fsdistr_random_exponential = 0

    # multiply mean size by this to get max file size

    random_size_limit = 8

    # large prime number used to randomly select directory given file number

    some_prime = 900593

    # build largest supported buffer, and fill it full of random hex digits,
    # then just use a substring of it below

    biggest_buf_size_bits = 20
    random_seg_size_bits = 10
    biggest_buf_size = 1 << biggest_buf_size_bits

    # initialize files with up to this many different random patterns
    buf_offset_range = 1 << 10

    loggers = {}  # so we only instantiate logger for a given thread name once

    # added by fuchen, here we record all the files generated by the fuzzer
    generated_files =[]

    # added by fuchen, here we record all the directories generated by the fuzzer
    generated_dirs = []

    # operations on the lists
    def add_generated_files(self, filename):
        if filename not in self.generated_files:
            self.generated_files.append(filename)
    
    def delete_generated_files(self, filename):
        if filename in self.generated_files:
            self.generated_files.remove(filename)

    def clear_generated_files(self):
        self.generated_files = []

    def add_generated_dirs(self, dirname):
        if dirname not in self.generated_dirs:
            self.generated_dirs.append(dirname)

    def delete_generated_dirs(self, dirname):
        if dirname in self.generated_dirs:
            self.generated_dirs.remove(dirname)
            # We also need to remove all the sub dirs and the files in that dir
            self.remove_sub_dir(dirname)
 
    def remove_sub_dir(self,start_dir):
        dir_res = os.listdir(start_dir)
        for path in dir_res:
            temp_path = start_dir + '/' + path
            if os.path.isfile(temp_path):
                self.delete_generated_files(temp_path)
            if os.path.isdir(temp_path):
                self.delete_generated_dirs(temp_path)
                self.remove_sub_dir(temp_path)

    def clear_generated_dirs(self):
        self.generated_dirs = []

    def random_choose_a_file(self):
        if self.generated_files == []:
            # currently, the generated files are empty
            return None
        else:
            chosen_file = random.choice(self.generated_files)
            return chosen_file

    def random_choose_a_dir(self):
        if self.generated_dirs == []:
            # currently, the generated dirs are empty
            return None
        else:
            chosen_dir = random.choice(self.generated_dirs)
            return chosen_dir

    # constructor sets up initial, default values for test parameters
    # user overrides these values using CLI interface parameters
    # for boolean parameters,
    # preceding comment describes what happens if parameter is set to True

    def __init__(self):
        # all threads share same directory
        self.is_shared_dir = False

        # file operation type, default idempotent
        self.opname = "cleanup"

        # how many files accessed, default = quick test
        self.iterations = 200

        # top of directory tree, default always exists on local fs
        top = join(self.tmp_dir, "smf")

        # file that tells thread when to start running
        self.starting_gate = None

        # transfer size (KB), 0 = default to file size
        self.record_sz_kb = 0

        # total data read/written in KB
        self.total_sz_kb = 102400

        # file size distribution, default = all files same size
        # self.filesize_distr = self.fsdistr_fixed
        self.filesize_distr = self.fsdistr_random_exponential

        # how many directories to use
        self.files_per_dir = 100

        # fanout if > 1 dir/thread needed
        self.dirs_per_dir = 10

        # size of xattrs to read/write
        self.xattr_size = 0

        # number of xattrs to read/write
        self.xattr_count = 0

        # test-over polling rate
        self.files_between_checks = 20

        # prepend this to file name
        self.prefix = ""

        # append this to file name
        self.suffix = ""

        # directories are accessed randomly
        self.hash_to_dir = False

        # fsync() issued after a file is modified
        self.fsync = False

        # update xattr with ctime+size
        self.record_ctime_size = False

        # end test as soon as any thread finishes
        self.stonewall = True

        # finish remaining requests after test ends
        self.finish_all_rq = False

        # append response times to .rsptimes
        self.measure_rsptimes = False

        # write/expect binary random (incompressible) data
        self.incompressible = False

        # , compare read data to what was written
        self.verify_read = True

        # should we attempt to adjust pause between files
        self.auto_pause = False

        # sleep this long between each file op
        self.pause_between_files = 0.0

        # collect samples for this long, then add to start time
        self.pause_history_duration = 1.0

        # wait this long after cleanup for async. deletion activity to finish
        self.cleanup_delay_usec_per_file = 0

        # which host the invocation ran on
        self.onhost = get_hostname(None)

        # thread ID
        self.tid = ""

        # debug to screen
        self.log_to_stderr = False

        # print debug messages
        self.verbose = True

        # create directories as needed
        self.dirs_on_demand = False

        # for internal use only

        # self.set_top([top])

        # self.file_tree = FileTree(top)

        # logging level, default is just informational, warning or error
        self.log_level = logging.INFO

        # will be initialized later with thread-safe python logging object
        self.log = None

        # buffer for reads and writes will be here
        self.buf = None

        # copy from here on writes, compare to here on reads
        self.biggest_buf = None

        # random seed used to control sequence of random numbers,
        # default to different sequence every time
        self.randstate = random.Random()

        # number of hosts/pods in test, default is 1 smallfile host/pod
        self.total_hosts = 1

        # number of threads in each host/pod
        self.threads = 1

        # reset object state variables

        self.reset()

    # FIXME: should be converted to dictionary and output in JSON
    # convert object to string for logging, etc.

    def reset_tree(self, new_path):
        self.file_tree = FileTree(new_path)

    def __str__(self):
        s = " opname=" + self.opname
        s += " iterations=" + str(self.iterations)
        s += " top_dirs=" + str(self.top_dirs)
        s += " src_dirs=" + str(self.src_dirs)
        s += " dest_dirs=" + str(self.dest_dirs)
        s += " network_dir=" + str(self.network_dir)
        s += " shared=" + str(self.is_shared_dir)
        s += " record_sz_kb=" + str(self.record_sz_kb)
        s += " total_sz_kb=" + str(self.total_sz_kb)
        s += " filesize_distr=" + str(self.filesize_distr)
        s += " files_per_dir=%d" % self.files_per_dir
        s += " dirs_per_dir=%d" % self.dirs_per_dir
        s += " dirs_on_demand=" + str(self.dirs_on_demand)
        s += " xattr_size=%d" % self.xattr_size
        s += " xattr_count=%d" % self.xattr_count
        s += " starting_gate=" + str(self.starting_gate)
        s += " prefix=" + self.prefix
        s += " suffix=" + self.suffix
        s += " hash_to_dir=" + str(self.hash_to_dir)
        s += " fsync=" + str(self.fsync)
        s += " stonewall=" + str(self.stonewall)
        s += " cleanup_delay_usec_per_file=" + str(self.cleanup_delay_usec_per_file)
        s += " files_between_checks=" + str(self.files_between_checks)
        s += " pause=" + str(self.pause_between_files)
        s += " pause_sec=" + str(self.pause_sec)
        s += " auto_pause=" + str(self.auto_pause)
        s += " verify_read=" + str(self.verify_read)
        s += " incompressible=" + str(self.incompressible)
        s += " finish_all_rq=" + str(self.finish_all_rq)
        s += " rsp_times=" + str(self.measure_rsptimes)
        s += " tid=" + self.tid
        s += " loglevel=" + str(self.log_level)
        s += " filenum=" + str(self.filenum)
        s += " filenum_final=" + str(self.filenum_final)
        s += " rq=" + str(self.rq)
        s += " rq_final=" + str(self.rq_final)
        s += " total_hosts=" + str(self.total_hosts)
        s += " threads=" + str(self.threads)
        s += " start=" + str(self.start_time)
        s += " end=" + str(self.end_time)
        s += " elapsed=" + str(self.elapsed_time)
        s += " host=" + str(self.onhost)
        s += " status=" + str(self.status)
        s += " abort=" + str(self.abort)
        s += " log_to_stderr=" + str(self.log_to_stderr)
        s += " verbose=" + str(self.verbose)
        return s

    # if you want to use the same instance for multiple tests
    # call reset() method between tests

    def reset(self):
        # results returned in variables below
        self.filenum = 0  # how many files have been accessed so far
        self.filenum_final = None  # how many files accessed when test ended
        self.rq = 0  # how many reads/writes have been attempted so far
        self.rq_final = None  # how many reads/writes completed when test ended
        self.abort = False
        self.file_dirs = []  # subdirectores within per-thread dir
        self.status = ok

        # response time samples for auto-pause feature
        self.pause_rsptime_count = 100
        # special value that means no response times have been measured yet
        self.pause_rsptime_unmeasured = -11
        self.files_between_pause = 5
        self.pause_rsptime_index = self.pause_rsptime_unmeasured
        self.pause_rsptime_history = [0 for k in range(0, self.pause_rsptime_count)]
        self.pause_sample_count = 0
        # start time for this history interval
        self.pause_history_start_time = 0.0
        self.pause_sec = self.pause_between_files / self.MICROSEC_PER_SEC
        # recalculate this to capture any changes in self.total_hosts and self.threads
        self.total_threads = self.total_hosts * self.threads
        self.throttling_factor = 0.1 * math.log(self.total_threads + 1, 2)

        # to measure per-thread elapsed time
        self.start_time = None
        self.end_time = None
        self.elapsed_time = None

        # to measure file operation response times
        self.op_start_time = None
        self.rsptimes = []
        self.rsptime_filename = None

    # given a set of top-level directories (e.g. for NFS benchmarking)
    # set up shop in them
    # we only use one directory for network synchronization

    def set_top(self, top_dirs, network_dir=None):
        self.top_dirs = top_dirs
        # create/read files here
        self.src_dirs = [join(d, "file_srcdir") for d in top_dirs]
        # rename files to here
        self.dest_dirs = [join(d, "file_dstdir") for d in top_dirs]

        # directory for synchronization files shared across hosts
        self.network_dir = join(top_dirs[0], "network_shared")
        if network_dir:
            self.network_dir = network_dir

    def create_top_dirs(self, is_multi_host):
        if os.path.exists(self.network_dir):
            rmtree(self.network_dir)
            if is_multi_host:
                # so all remote clients see that directory was recreated
                time.sleep(2.1)
        ensure_dir_exists(self.network_dir)
        for dlist in [self.src_dirs, self.dest_dirs]:
            for d in dlist:
                ensure_dir_exists(d)
        if is_multi_host:
            # workaround to force cross-host synchronization
            time.sleep(1.1)  # lets NFS mount option actimeo=1 take effect
            os.listdir(self.network_dir)

    # create per-thread log file
    # we have to avoid getting the logger for self.tid more than once,
    # or else we'll add a handler more than once to this logger
    # and cause duplicate log messages in per-invoke log file

    def start_log(self):
        try:
            self.log = self.loggers[self.tid]
        except KeyError:
            self.log = logging.getLogger(self.tid)
            self.loggers[self.tid] = self.log
            if self.log_to_stderr:
                h = logging.StreamHandler()
            else:
                h = logging.FileHandler(self.log_fn())
            log_format = self.tid + " %(asctime)s - %(levelname)s - %(message)s"
            formatter = logging.Formatter(log_format)
            h.setFormatter(formatter)
            self.log.addHandler(h)
        self.loglevel = logging.INFO
        if self.verbose:
            self.loglevel = logging.DEBUG
        self.log.setLevel(self.loglevel)

    # indicate start of an operation

    def op_starttime(self, starttime=None):
        if not starttime:
            self.op_start_time = time.time()
        else:
            self.op_start_time = starttime

    # indicate end of an operation,
    # this appends the elapsed time of the operation to .rsptimes array

    def op_endtime(self, opname):
        end_time = time.time()
        rsp_time = end_time - self.op_start_time
        if self.measure_rsptimes:
            self.rsptimes.append((opname, self.op_start_time, rsp_time))
        self.op_start_time = None
        if self.auto_pause:
            self.adjust_pause_time(end_time, rsp_time)

    # save response times seen by this thread

    def save_rsptimes(self):
        fname = "rsptimes_{tid}_{host}_{op}_{ts}.csv".format(
            tid=str(self.tid),
            host=get_hostname(None),
            op=self.opname,
            ts=str(self.start_time),
        )
        rsptime_fname = join(self.network_dir, fname)
        with open(rsptime_fname, "w") as f:
            for opname, start_time, rsp_time in self.rsptimes:
                # time granularity is microseconds, accuracy is less
                f.write(
                    "%8s, %9.6f, %9.6f\n"
                    % (opname, start_time - self.start_time, rsp_time)
                )
            os.fsync(f.fileno())  # particularly for NFS this is needed

    # compute pause time based on available response time samples,
    # assuming all threads converge to roughly the same average response time
    # we treat the whole system as one big queueing center and apply
    # little's law U = XS to it to estimate what pause time should be
    # to achieve max throughput without excessive queueing and unfairness

    def calculate_pause_time(self, end_time):
        # there are samples to process
        mean_rsptime = sum(self.pause_rsptime_history) / self.pause_rsptime_count
        time_so_far = end_time - self.pause_history_start_time
        # estimate system throughput assuming all threads are same
        # per-thread throughput is measured by number of rsptime samples
        # in this interval divided by length of interval
        est_throughput = self.pause_sample_count * self.total_threads / time_so_far
        # assumption: all threads converge to the same throughput
        mean_utilization = mean_rsptime * est_throughput
        old_pause = self.pause_sec
        new_pause = mean_utilization * mean_rsptime * self.throttling_factor
        self.pause_sec = (old_pause + 2 * new_pause) / 3.0
        self.log.debug(
            "time_so_far %f samples %d index %d mean_rsptime %f throttle %f est_throughput %f mean_util %f"
            % (
                time_so_far,
                self.pause_sample_count,
                self.pause_rsptime_index,
                mean_rsptime,
                self.throttling_factor,
                est_throughput,
                mean_utilization,
            )
        )
        self.log.info(
            "per-thread pause changed from %9.6f to %9.6f" % (old_pause, self.pause_sec)
        )

    # adjust pause time based on whether response time was significantly bigger than pause time
    # we lower the pause time until

    def adjust_pause_time(self, end_time, rsp_time):
        self.log.debug(
            "adjust_pause_time %f %f %f %f"
            % (end_time, rsp_time, self.pause_sec, self.pause_history_start_time)
        )
        if self.pause_rsptime_index == self.pause_rsptime_unmeasured:
            self.pause_sec = 0.00001
            self.pause_history_start_time = end_time - rsp_time
            # try to get the right order of magnitude for response time estimate immediately
            self.pause_rsptime_history = [
                rsp_time for k in range(0, self.pause_rsptime_count)
            ]
            self.pause_rsptime_index = 1
            self.pause_sample_count = 1
            self.pause_sec = self.throttling_factor * rsp_time
            # self.calculate_pause_time(end_time)
            self.log.info("per-thread pause initialized to %9.6f" % self.pause_sec)
        else:
            # insert response time into ring buffer of most recent response times
            self.pause_rsptime_history[self.pause_rsptime_index] = rsp_time
            self.pause_rsptime_index += 1
            if self.pause_rsptime_index >= self.pause_rsptime_count:
                self.pause_rsptime_index = 0
            self.pause_sample_count += 1

            # if it's time to adjust pause_sec...
            if (
                self.pause_history_start_time + self.pause_history_duration < end_time
                or self.pause_sample_count > self.pause_rsptime_count / 2
            ):
                self.calculate_pause_time(end_time)
                self.pause_history_start_time = end_time
                self.pause_sample_count = 0

    # determine if test interval is over for this thread

    # each thread uses this to signal that it is at the starting gate
    # (i.e. it is ready to immediately begin generating workload)

    def gen_thread_ready_fname(self, tid, hostname=None):
        return join(self.tmp_dir, "thread_ready." + tid + ".tmp")

    # each host uses this to signal that it is
    # ready to immediately begin generating workload
    # each host places this file in a directory shared by all hosts
    # to indicate that this host is ready

    def gen_host_ready_fname(self, hostname=None):
        if not hostname:
            hostname = self.onhost
        return join(self.network_dir, "host_ready." + hostname + ".tmp")

    # abort file tells other threads not to start test
    # because something has already gone wrong

    def abort_fn(self):
        return join(self.network_dir, "abort.tmp")

    # stonewall file stops test measurement
    # (does not stop worker thread unless --finish N is used)

    def stonewall_fn(self):
        return join(self.network_dir, "stonewall.tmp")

    # log file for this worker thread goes here

    def log_fn(self):
        return join(self.tmp_dir, "invoke_logs-%s.log" % self.tid)

    # file for result stored as pickled python object

    def host_result_filename(self, result_host=None):
        if result_host is None:
            result_host = self.onhost
        return join(self.network_dir, result_host + "_result.pickle")

    # we use the seed function to control per-thread random sequence
    # we want seed to be saved
    # so that operations subsequent to initial create will know
    # what file size is for thread T's file j without having to stat the file

    def init_random_seed(self):
        fn = self.gen_thread_ready_fname(self.tid, hostname=self.onhost) + ".seed"
        thread_seed = str(time.time())
        self.log.debug("seed opname: " + self.opname)
        if self.opname == "create" or self.opname == "swift-put":
            thread_seed = str(time.time()) + " " + self.tid
            ensure_deleted(fn)
            with open(fn, "w") as seedfile:
                seedfile.write(str(thread_seed))
                self.log.debug("write seed %s " % thread_seed)
        # elif ['append', 'read', 'swift-get'].__contains__(self.opname):
        else:
            try:
                with open(fn, "r") as seedfile:
                    thread_seed = seedfile.readlines()[0].strip()
                    self.log.debug("read seed %s " % thread_seed)
            except OSError as e:
                if e.errno == errno.ENOENT and self.opname in [
                    "cleanup",
                    "rmdir",
                    "delete",
                ]:
                    self.log.info(
                        "no saved random seed found in %s but it does not matter for deletes"
                        % fn
                    )
        self.randstate.seed(thread_seed)

    def get_next_file_size(self):
        next_size = self.total_sz_kb
        if self.filesize_distr == self.fsdistr_random_exponential:
            next_size = max(
                1,
                min(
                    int(self.randstate.expovariate(1.0 / self.total_sz_kb)),
                    self.total_sz_kb * self.random_size_limit,
                ),
            )
            if self.log_level == logging.DEBUG:
                self.log.debug("rnd expn file size %d KB" % next_size)
            else:
                self.log.debug("fixed file size %d KB" % next_size)
        return next_size

    def get_file_size(self, filename):
        file_stat = os.stat(filename)
        return file_stat.st_size

    # tell test driver that we're at the starting gate
    # this is a 2 phase process
    # first wait for each thread on this host to reach starting gate
    # second, wait for each host in test to reach starting gate
    # in case we have a lot of threads/hosts, sleep 1 sec between polls
    # also, wait 2 sec after seeing starting gate to maximize probability
    # that other hosts will also see it at the same time

    def wait_for_gate(self):
        if self.starting_gate:
            gateReady = self.gen_thread_ready_fname(self.tid)
            touch(gateReady)
            delay_time = 0.1
            while not os.path.exists(self.starting_gate):
                if os.path.exists(self.abort_fn()):
                    raise SMFRunException("thread " + str(self.tid) + " saw abort flag")
                # wait a little longer so that
                # other clients have time to see that gate exists
                delay_time = delay_time * 1.5
                if delay_time > 2.0:
                    delay_time = 2.0
                time.sleep(delay_time)
            gateinfo = os.stat(self.starting_gate)
            synch_time = gateinfo.st_mtime + 3.0 - time.time()
            if synch_time > 0.0:
                time.sleep(synch_time)
            if synch_time < 0.0:
                self.log.warn("other threads may have already started")
            if self.verbose:
                self.log.debug(
                    "started test at %f sec after waiting %f sec"
                    % (time.time(), synch_time)
                )

    # record info needed to compute test statistics

    def end_test(self):
        # be sure end_test is not called more than once
        # during do_workload()
        if self.test_ended():
            return
        myassert(
            self.end_time is None
            and self.rq_final is None
            and self.filenum_final is None
        )
        self.rq_final = self.rq
        self.filenum_final = self.filenum
        self.end_time = time.time()
        self.elapsed_time = self.end_time - self.start_time
        stonewall_path = self.stonewall_fn()
        if self.filenum >= self.iterations and not os.path.exists(stonewall_path):
            try:
                touch(stonewall_path)
                self.log.info("stonewall file %s written" % stonewall_path)
            except IOError as e:
                err = e.errno
                if err != errno.EEXIST:
                    # workaround for possible bug in Gluster
                    if err != errno.EINVAL:
                        self.log.error(
                            "unable to write stonewall file %s" % stonewall_path
                        )
                        self.log.exception(e)
                        self.status = err
                    else:
                        self.log.info("saw EINVAL on stonewall, ignoring it")

    def test_ended(self):
        return (self.end_time is not None) and (self.end_time > self.start_time)

    # see if we should do one more file
    # to minimize overhead, do not check stonewall file before every iteration

    def do_another_file(self):
        if self.stonewall and (((self.filenum + 1) % self.files_between_checks) == 0):
            stonewall_path = self.stonewall_fn()
            if self.verbose:
                self.log.debug(
                    "checking for stonewall file %s after %s iterations"
                    % (stonewall_path, self.filenum)
                )
            if os.path.exists(stonewall_path):
                self.log.info(
                    "stonewall file %s seen after %d iterations"
                    % (stonewall_path, self.filenum)
                )
                self.end_test()

        # if user doesn't want to finish all requests and test has ended, stop

        if not self.finish_all_rq and self.test_ended():
            return False
        if self.status != ok:
            self.end_test()
            return False
        if self.filenum >= self.iterations:
            self.end_test()
            return False
        if self.abort:
            raise SMFRunException("thread " + str(self.tid) + " saw abort flag")
        self.filenum += 1
        if self.pause_sec > 0.0 and self.iterations % self.files_between_pause == 0:
            time.sleep(self.pause_sec * self.files_between_pause)
        return True

    # in this method of directory selection, as filenum increments upwards,
    # we place F = files_per_dir files into directory,
    # then next F files into directory D+1, etc.
    # we generate directory pathnames like radix-D numbers
    # where D is subdirectories per directory
    # see URL http://gmplib.org/manual/Binary-to-Radix.html#Binary-to-Radix
    # this algorithm should take O(log(F))

    def mk_seq_dir_name(self, file_num):
        dir_in = file_num // self.files_per_dir
        # generate powers of self.files_per_dir not greater than dir_in
        level_dirs = []
        dirs_for_this_level = self.dirs_per_dir
        while dirs_for_this_level <= dir_in:
            level_dirs.append(dirs_for_this_level)
            dirs_for_this_level *= self.dirs_per_dir

        # generate each "digit" in radix-D number as result of quotients
        # from dividing remainder by next lower power of D (think of base 10)

        levels = len(level_dirs)
        level = levels - 1
        pathlist = []
        while level > -1:
            dirs_in_level = level_dirs[level]
            quotient = dir_in // dirs_in_level
            dir_in = dir_in - quotient * dirs_in_level
            dirnm = "d_" + str(quotient).zfill(3)
            pathlist.append(dirnm)
            level -= 1
        pathlist.append("d_" + str(dir_in).zfill(3))
        return os.sep.join(pathlist)

    def mk_hashed_dir_name(self, file_num):
        pathlist = []
        random_hash = file_num * self.some_prime % self.iterations
        dir_num = random_hash // self.files_per_dir
        while dir_num > 1:
            dir_num_hash = dir_num * self.some_prime % self.dirs_per_dir
            pathlist.insert(0, "h_" + str(dir_num_hash).zfill(3))
            dir_num //= self.dirs_per_dir
        return os.sep.join(pathlist)

    def mk_dir_name(self, file_num):
        if self.hash_to_dir:
            return self.mk_hashed_dir_name(file_num)
        else:
            return self.mk_seq_dir_name(file_num)

    # generate file name to put in this directory
    # prefix can be used for process ID or host ID for example
    # names are unique to each thread
    # automatically computes subdirectory for file based on
    # files_per_dir, dirs_per_dir and placing file as high in tree as possible
    # for multiple-mountpoint tests,
    # we need to select top-level dir based on file number
    # to spread load across mountpoints,
    # so we use round-robin mountpoint selection
    # NOTE: this routine is called A LOT,
    # so need to optimize by avoiding lots of os.path.join calls

    def mk_file_nm(self, base_dirs, filenum=-1):
        if filenum == -1:
            filenum = self.filenum
        listlen = len(base_dirs)
        tree = base_dirs[filenum % listlen]
        components = [
            tree,
            os.sep,
            self.file_dirs[filenum],
            os.sep,
            self.prefix,
            "_",
            self.onhost,
            "_",
            self.tid,
            "_",
            str(filenum),
            "_",
            self.suffix,
            "_",
            str(int(time.time()))
        ]
        return "".join(components)

    # generate buffer contents, use these on writes and
    # compare against them for reads where random data is used,

    def create_biggest_buf(self, contents_random):
        # generate random byte sequence if desired.

        random_segment_size = 1 << self.random_seg_size_bits
        if not self.incompressible:
            # generate a random byte sequence of length 2^random_seg_size_bits
            # and then repeat the sequence
            # until we get to size 2^biggest_buf_size_bits in length

            if contents_random:
                biggest_buf = bytearray(
                    [
                        self.randstate.randrange(0, 127)
                        for k in range(0, random_segment_size)
                    ]
                )
            else:
                biggest_buf = bytearray(
                    [k % 128 for k in range(0, random_segment_size)]
                )

            # to prevent confusion in python when printing out buffer contents
            # WARNING: this line breaks PythonTidy utility
            biggest_buf = biggest_buf.replace(b"\\", b"!")

            # keep doubling buffer size until it is big enough

            next_power_2 = self.biggest_buf_size_bits - self.random_seg_size_bits
            for j in range(0, next_power_2):
                biggest_buf.extend(biggest_buf[:])

        else:  # if incompressible
            # for buffer to be incompressible,
            # we can't repeat the same (small) random sequence
            # FIXME: why shouldn't we always do it this way?

            # initialize to a single random byte
            biggest_buf = bytearray([self.randstate.randrange(0, 255)])
            myassert(len(biggest_buf) == 1)
            powerof2 = 1
            powersum = 1
            for j in range(0, self.biggest_buf_size_bits - 1):
                myassert(len(biggest_buf) == powersum)
                powerof2 *= 2
                powersum += powerof2
                # biggest_buf length is now 2^j - 1
                biggest_buf.extend(
                    bytearray(
                        [self.randstate.randrange(0, 255) for k in range(0, powerof2)]
                    )
                )
            biggest_buf.extend(bytearray([self.randstate.randrange(0, 255)]))

        # add extra space at end
        # so that we can get different buffer contents
        # by just using different offset into biggest_buf

        biggest_buf.extend(biggest_buf[0 : self.buf_offset_range])
        myassert(len(biggest_buf) == self.biggest_buf_size + self.buf_offset_range)
        return biggest_buf

    # allocate buffer of correct size with offset based on filenum, tid, etc.

    def prepare_buf(self):
        # determine max record size of I/Os

        total_space_kb = self.record_sz_kb
        if self.record_sz_kb == 0:
            if self.filesize_distr != self.fsdistr_fixed:
                total_space_kb = self.total_sz_kb * self.random_size_limit
            else:
                total_space_kb = self.total_sz_kb

        total_space = total_space_kb * self.BYTES_PER_KB
        if total_space > Eris.biggest_buf_size:
            total_space = Eris.biggest_buf_size

        # ensure pre-allocated pre-initialized buffer space
        # big enough for xattr ops
        # use +, not *, see way buffers are used

        total_xattr_space = self.xattr_size + self.xattr_count
        if total_xattr_space > total_space:
            total_space = total_xattr_space

        # create a buffer with somewhat unique contents for this file,
        # so we'll know if there is a read error
        # unique_offset has to have same value across smallfile runs
        # so that we can write data and then
        # know what to expect in written data later on
        # NOTE: this means self.biggest_buf must be
        # 1K larger than Eris.biggest_buf_size

        max_buffer_offset = 1 << 10
        try:
            unique_offset = ((int(self.tid) + 1) * self.filenum) % max_buffer_offset
        except ValueError:
            unique_offset = self.filenum % max_buffer_offset
        myassert(total_space + unique_offset < len(self.biggest_buf))
        # if self.verbose:
        #    self.log.debug('unique_offset: %d' % unique_offset)

        self.buf = self.biggest_buf[unique_offset : total_space + unique_offset]
        # if self.verbose:
        #    self.log.debug('start of prepared buf: %s' % self.buf.hex()[0:40])

    # determine record size to use in test
    # if record size is 0, that means to use largest possible value
    # we try to use the file size as the record size, but
    # if the biggest_buf_size is less than the file size, use it instead.

    def get_record_size_to_use(self):
        rszkb = self.record_sz_kb
        if rszkb == 0:
            rszkb = self.total_sz_kb
        if rszkb > Eris.biggest_buf_size // self.BYTES_PER_KB:
            rszkb = Eris.biggest_buf_size // self.BYTES_PER_KB
        return rszkb

    # make all subdirectories needed for test in advance,
    # don't include in measurement
    # use set to avoid duplicating operations on directories

    def make_all_subdirs(self):
        self.log.debug("making all subdirs")
        abort_filename = self.abort_fn()
        if self.tid != "00" and self.is_shared_dir:
            return
        dirset = set()

        # FIXME: we could check to see if
        # self.dest_dirs is actually used before we include it

        for tree in [self.src_dirs, self.dest_dirs]:
            tree_range = range(0, len(tree))

            # if we are hashing into directories,
            # we can't make any assumptions about
            # which directories will be used first

            if self.hash_to_dir:
                dir_range = range(0, self.iterations + 1)
            else:
                # optimization: if not hashing into directories,
                # we put files_per_dir files into each directory, so
                # we only need to check every files_per_dir filenames
                # for a new directory name
                dir_range = range(
                    0, self.iterations + self.files_per_dir, self.files_per_dir
                )

            # we need this range because
            # we need to create directories in each top dir
            for k in tree_range:
                for j in dir_range:
                    fpath = self.mk_file_nm(tree, j + k)
                    dpath = os.path.dirname(fpath)
                    dirset.add(dpath)

        # since we put them into a set, duplicates are filtered out

        for unique_dpath in dirset:
            if exists(abort_filename):
                break
            if not exists(unique_dpath):
                try:
                    os.makedirs(unique_dpath, 0o777)
                    if debug_timeout:
                        time.sleep(1)
                except OSError as e:
                    if not (e.errno == errno.EEXIST and self.is_shared_dir):
                        raise e

    # operation-specific test code goes in do_<opname>()
    # whatever record size sequence we use in do_create
    # must also be attempted in do_read

    def do_create(self, opRands):
        fn = opRands[0]
        self.op_starttime()
        fd = -1
        try:
            fd = os.open(fn, os.O_CREAT | os.O_EXCL | os.O_WRONLY | O_BINARY)
            time.sleep(1)
            self.add_generated_files(fn)
            self.log.info(
                "COMMAND: fd = os.open(%s,os.O_CREAT | os.O_EXCL | os.O_WRONLY | O_BINARY)"
                % fn
            )
            if fd < 0:
                self.log.error("failed to open file %s" % fn)
                raise MFRdWrExc(self.opname, self.filenum, 0, 0)
            remaining_kb = self.get_next_file_size()
            self.prepare_buf()
            rszkb = self.get_record_size_to_use()
            while remaining_kb > 0:
                next_kb = min(rszkb, remaining_kb)
                rszbytes = next_kb * self.BYTES_PER_KB
                written = os.write(fd, self.buf[0:rszbytes])
                self.log.info(
                    "COMMAND: os.write(fd, self.buf[0:%d])"
                    % rszbytes
                )
                if written != rszbytes:
                    raise MFRdWrExc(self.opname, self.filenum, self.rq, written)
                self.rq += 1
                remaining_kb -= next_kb
            if self.record_ctime_size:
                remember_ctime_size_xattr(fd)
        # except OSError as e:
        #     if e.errno == errno.ENOENT and self.dirs_on_demand:
        #         os.makedirs(os.path.dirname(fn))
        #         self.filenum -= 1  # retry this file now that dir. exists
        #         continue
        #     self.status = e.errno
        #     raise e
        finally:
            if fd >= 0:
                if self.fsync:
                    os.fsync(fd)
                os.close(fd)
        self.op_endtime(self.opname)

    def do_mkdir(self, opRands):
        dir_name = opRands[0]
        self.op_starttime()
        try:
            self.add_generated_dirs(dir)
            os.mkdir(dir_name)
            self.log.info(
                "COMMAND: os.mkdir(%s)"
                % dir
            )
            time.sleep(1)
        # except OSError as e:
        #     if e.errno == errno.ENOENT and self.dirs_on_demand:
        #         os.makedirs(os.path.dirname(dir))
        #         self.filenum -= 1
        #         continue
        #     raise e
        finally:
            self.op_endtime(self.opname)

    def do_rmdir(self, opRands):
        # dir = self.mk_file_nm(self.src_dirs) + ".d"
        # dir = self.random_choose_a_dir()
        dir = opRands[0]
        self.op_starttime()
        self.delete_generated_dirs(dir)
        if "test_dfs_fuzzer" in dir:
            os.system("rm -rf " + dir)
            self.log.info(
                "COMMAND: os.rmdir(%s)"
                % dir
            )
        self.op_endtime(self.opname)

    def do_symlink(self, opRands):
        # chosen_file = self.random_choose_a_file()
        # fn = self.mk_file_nm(self.src_dirs)
        # fn2 = self.mk_file_nm(self.dest_dirs) + ".s"
        fn = opRands[0]
        fn2 = opRands[1]
        self.op_starttime()
        os.symlink(fn, fn2)
        time.sleep(1)
        # self.add_generated_files(fn2)
        self.log.info(
            "COMMAND: os.symlink(%s, %s)"
            % (fn, fn2)
        )
        self.op_endtime(self.opname)

    def do_hardlink(self, opRands):
        # chosen_file = self.random_choose_a_file()
        # fn = self.mk_file_nm(self.src_dirs)
        # fn2 = self.mk_file_nm(self.dest_dirs) + ".s"
        fn = opRands[0]
        fn2 = opRands[1]
        self.op_starttime()
        os.link(fn, fn2)
        time.sleep(1)
        # self.add_generated_files(fn2)
        self.log.info(
            "COMMAND: os.link(%s, %s)"
            % (fn, fn2)
        )
        self.op_endtime(self.opname)

    def do_chmod(self,opRands):
        # fn = self.mk_file_nm(self.src_dirs)
        fn = opRands[0]
        self.op_starttime()
        list_of_mode = [stat.S_ISUID,stat.S_ISGID,stat.S_ENFMT,stat.S_ISVTX,stat.S_IREAD,stat.S_IWRITE,stat.S_IEXEC,stat.S_IRWXU,stat.S_IRUSR,stat.S_IWUSR,stat.S_IXUSR,stat.S_IRWXG,stat.S_IRGRP,stat.S_IWGRP,stat.S_IXGRP,stat.S_IRWXO,stat.S_IROTH,stat.S_IWOTH,stat.S_IXOTH]
        selected_mode = random.choice(list_of_mode)
        # os.chmod(fn, 0o646)
        os.chmod(fn, selected_mode)
        self.log.info(
            "COMMAND: os.chmod(%s,%d)"
            % (fn,selected_mode)
        )
        self.op_endtime(self.opname)

    def do_append(self,opRands):
        return self.do_write(opRands,append=True)

    def do_overwrite(self, opRands):
        return self.do_write(opRands)

    def do_truncate_overwrite(self, opRands):
        return self.do_write(opRands, truncate=True)

    def do_write(self, opRands, append=False, truncate=False):
        if self.record_ctime_size and not xattr_installed:
            raise SMFRunException(
                "xattr module not present but record-ctime-size specified"
            )
        if append and truncate:
            raise SMFRunException("can not append and truncate at the same time")

        fn = opRands[0]
        self.op_starttime()
        fd = -1
        try:
            # don't use O_APPEND, it has different semantics!
            open_mode = os.O_WRONLY | O_BINARY
            if truncate:
                open_mode |= os.O_TRUNC
            fd = os.open(fn, open_mode)
            self.log.info(
                "COMMAND: fd = os.open(%s,%d)"
                % (fn,open_mode)
            )
            if append:
                os.lseek(fd, 0, os.SEEK_END)
                self.log.info(
                    "COMMAND: os.lseek(fd, 0, os.SEEK_END)"
                )
            remaining_kb = self.get_next_file_size()
            # print("Next file's size is:", remaining_kb)
            self.prepare_buf()
            rszkb = self.get_record_size_to_use()
            while remaining_kb > 0:
                next_kb = min(remaining_kb, rszkb)
                rszbytes = next_kb * self.BYTES_PER_KB
                written = os.write(fd, self.buf[0:rszbytes])
                self.log.info(
                    "COMMAND: os.write(fd, self.buf[0:%d])"
                    % rszbytes
                )
                self.rq += 1
                if written != rszbytes:
                    raise MFRdWrExc(self.opname, self.filenum, self.rq, written)
                remaining_kb -= next_kb
            if self.record_ctime_size:
                remember_ctime_size_xattr(fd)
            if self.fsync:
                os.fsync(fd)
                self.log.info(
                    "COMMAND: os.fsync(fd)"
                )
        finally:
            if fd >= 0:
                os.close(fd)
                self.log.info(
                    "COMMAND: os.close(fd)"
                )
        self.op_endtime(self.opname)

    def do_read(self, opRands):
        # fn = self.mk_file_nm(self.src_dirs)
        fn = self.opRands[0]
        # DEBUG:file p_cephclient-Standard-PC-i440FX-PIIX-1996_regtest_1_s_1687313576 read
        self.op_starttime()
        fd = -1
        try:
            # next_fsz = self.get_next_file_size()
            next_fsz = self.get_file_size(fn)
            fd = os.open(fn, os.O_RDONLY | O_BINARY)
            self.log.info(
                "COMMAND: fd = os.open(%s, os.O_RDONLY | O_BINARY)"
                % fn
            )
            self.prepare_buf()
            rszkb = self.get_record_size_to_use()
            remaining_kb = next_fsz/1024
            while remaining_kb > 0:
                next_kb = min(rszkb, remaining_kb)
                rszbytes = int(next_kb * self.BYTES_PER_KB)
                bytesread = os.read(fd, rszbytes)
                self.log.debug(
                    "rszkb is %d, remaining_kb is %d and next_kb is %d" %
                    (rszkb, remaining_kb, next_kb)
                )
                self.log.info(
                    "COMMAND: os.read(fd, %d)"
                    % rszbytes
                )
                self.rq += 1
                # if len(bytesread) != rszbytes:
                #     raise MFRdWrExc(
                #         self.opname, self.filenum, self.rq, len(bytesread)
                #     )
                if self.verify_read:
                    # this is in fast path so avoid evaluating self.log.debug
                    # unless people really want to see it
                    if self.verbose:
                        self.log.debug(
                            "read fn %s next_fsz %u remain %u rszbytes %u bytesread %u"
                            % (fn, next_fsz, remaining_kb, rszbytes, len(bytesread))
                        )
                    if self.buf[0:rszbytes] != bytesread:
                        bytes_matched = len(bytesread)
                        for k in range(0, rszbytes):
                            if self.buf[k] != bytesread[k]:
                                bytes_matched = k
                                break
                        # self.log.debug('front of read buffer: %s' % bytesread.hex()[0:40])
                        raise MFRdWrExc(
                            "read: buffer contents matched up through byte %d"
                            % bytes_matched,
                            self.filenum,
                            self.rq,
                            len(bytesread),
                        )
                remaining_kb -= next_kb
        finally:
            if fd > -1:
                os.close(fd)
                self.log.info(
                    "COMMAND: os.close(fd)"
                )
        self.op_endtime(self.opname)

    def do_readdir(self, opRands):
        if self.hash_to_dir:
            raise SMFRunException("cannot do readdir test with --hash-into-dirs option")
        prev_dir = ""
        dir_map = {}
        file_count = 0

        fn = opRands[0]
        dir = os.path.dirname(fn)
        common_dir = None
        for d in self.top_dirs:
            if dir.startswith(d):
                common_dir = dir[len(self.top_dirs[0]) :]
                break
        if not common_dir:
            raise SMFRunException(
                "readdir: filename %s is not in any top dir in %s"
                % (fn, str(self.top_dirs))
            )
        if common_dir != prev_dir:
            if file_count != len(dir_map):
                raise MFRdWrExc(
                    ("readdir: not all files in " + "directory %s were found")
                    % prev_dir,
                    self.filenum,
                    self.rq,
                    0,
                )
            self.op_starttime()
            dir_contents = []
            for t in self.top_dirs:
                next_dir = t + common_dir
                dir_contents.extend(os.listdir(next_dir))
            self.op_endtime(self.opname)
            prev_dir = common_dir
            dir_map = {}
            for listdir_filename in dir_contents:
                if not listdir_filename[0] == "d":
                    dir_map[listdir_filename] = True  # only include files
            file_count = 0
        if not fn.startswith("d"):
            file_count += 1  # only count files, not directories
        if os.path.basename(fn) not in dir_map:
            raise MFRdWrExc(
                "readdir: file missing from directory %s" % prev_dir,
                self.filenum,
                self.rq,
                0,
            )

    # this operation simulates a user doing "ls -lR" on a big directory tree
    # eventually we'll be able to use readdirplus() system call
    # if python supports it?

    def do_rename(self, opRands):
        in_same_dir = self.dest_dirs == self.src_dirs
        fn1 = opRands[0]
        fn2 = opRands[1]
        # if in_same_dir:
        #     fn2 = fn2 + self.rename_suffix
        self.op_starttime()
        os.rename(fn1, fn2)
        self.delete_generated_files(fn1)
        self.add_generated_files(fn2)
        self.op_endtime(self.opname)

    def do_delete(self, opRands):
        fn = opRands[0]
        self.op_starttime()
        os.unlink(fn)
        self.delete_generated_files(fn)
        self.op_endtime(self.opname)

    # unlike other ops, cleanup must always finish regardless of other threads

    def do_cleanup(self, opRands):
        # print(self.dest_dirs[0])
        # print(self.src_dirs[0])
        print(self.top_dirs)
        if "test_dfs_fuzzer" in self.top_dirs[0]:
            os.system("rm -rf "+self.top_dirs[0]+"/*")
            # os.system("rm -rf /mnt/mycephfs/test_dfs_fuzzer/invoke_logs-regtest.log")

            self.genrated_dirs = []
            self.generated_files = []

            self.log.info(
                "COMMAND: cleanup"
            )

    def old_do_workload(self):
        self.reset()
        for j in range(0, self.iterations + self.files_per_dir):
            self.file_dirs.append(self.mk_dir_name(j))
        self.start_log()
        self.log.info("do_workload: " + str(self))
        ensure_dir_exists(self.network_dir)
        if ["create", "mkdir", "swift-put"].__contains__(self.opname):
            self.make_all_subdirs()
        # create_biggest_buf() depends on init_random_seed()
        self.init_random_seed()
        self.biggest_buf = self.create_biggest_buf(False)
        if self.total_sz_kb > 0:
            self.files_between_checks = max(
                10, int(self.max_files_between_checks - self.total_sz_kb / 100)
            )
        try:
            self.wait_for_gate()
            self.start_time = time.time()
            o = self.opname
            func = Eris.workloads[o]
            func(self)  # call the do_ function for that workload type
        except KeyError as e:
            self.log.error("invalid workload type " + o)
            self.status = e.ENOKEY
        except KeyboardInterrupt as e:
            self.log.error("control-C (SIGINT) signal received, ending test")
            self.status = e.EINTR
        except OSError as e:
            self.status = e.errno
            self.log.error("OSError status %d seen" % e.errno)
            self.log.exception(e)
        except MFRdWrExc as e:
            self.status = errno.EIO
            self.log.error("MFRdWrExc seen")
            self.log.exception(e)
        if self.measure_rsptimes:
            self.save_rsptimes()
        if self.status != ok:
            self.log.error("invocation did not complete cleanly")
        if self.filenum != self.iterations:
            self.log.info("recorded throughput after " + str(self.filenum) + " files")
        self.log.info("finished %s" % self.opname)
        # this next call works fine with python 2.7
        # but not with python 2.6, why? do we need it?
        #    logging.shutdown()

        return self.status

    def do_workload(self,opRands, ano):
        self.start_log()
        self.log.info("do_workload: " + str(self) + ", with the anomaly:" + ano.value)
        # create_biggest_buf() depends on init_random_seed()
        self.biggest_buf = self.create_biggest_buf(False)
        if self.total_sz_kb > 0:
            self.files_between_checks = max(
                10, int(self.max_files_between_checks - self.total_sz_kb / 100)
            )
        try:
            self.start_time = time.time()
            o = self.opname
            func = Eris.workloads[o]
            func(self,opRands)  # call the do_ function for that workload type
            # TODO: re-write each do functions to support opRands
        except KeyError as e:
            self.log.error("invalid workload type " + o)
            self.status = e.ENOKEY
        except KeyboardInterrupt as e:
            self.log.error("control-C (SIGINT) signal received, ending test")
            self.status = e.EINTR
        except OSError as e:
            self.status = e.errno
            self.log.error("OSError status %d seen" % e.errno)
            self.log.exception(e)
        except MFRdWrExc as e:
            self.status = errno.EIO
            self.log.error("MFRdWrExc seen")
            self.log.exception(e)
        if self.measure_rsptimes:
            self.save_rsptimes()
        if self.status != ok:
            self.log.error("invocation did not complete cleanly")
        if self.filenum != self.iterations:
            self.log.info("recorded throughput after " + str(self.filenum) + " files")
        self.log.info("finished %s" % self.opname)
        # this next call works fine with python 2.7
        # but not with python 2.6, why? do we need it?
        #    logging.shutdown()

        return self.status

    # we look up the function for the workload type
    # by workload name in this dictionary (hash table)

    workloads = {
        "create": do_create,
        "delete": do_delete,
        "symlink": do_symlink,
        "hardlink": do_hardlink,
        "mkdir": do_mkdir,
        "rmdir": do_rmdir,
        "readdir": do_readdir,
        "chmod": do_chmod,
        "append": do_append,
        "overwrite": do_overwrite,
        "truncate-overwrite": do_truncate_overwrite,
        "read": do_read,
        "rename": do_rename,
        "cleanup": do_cleanup,
    }

class Fuzzer():
    # run before every test
    def setUp(self):
        self.invok = Eris()
        self.invok.opname = "create"
        self.invok.iterations = 1
        self.invok.files_per_dir = 5
        self.invok.dirs_per_dir = 2
        self.invok.verbose = True
        self.invok.finish_all_rq = True

    def deltree(self, topdir):
        if not os.path.exists(topdir):
            return
        if not os.path.isdir(topdir):
            return
        for dir, subdirs, files in os.walk(topdir, topdown=False):
            for f in files:
                os.unlink(join(dir, f))
            for d in subdirs:
                os.rmdir(join(dir, d))
        os.rmdir(topdir)

    def chk_status(self):
        if self.invok.status != ok:
            raise SMFRunException(
                "test failed, check log file %s" % self.invok.log_fn()
            )

    def runTest(self, opName, opRands, ano=Ano.NORMAL):
        # ensure_deleted(self.invok.stonewall_fn())
        self.invok.opname = opName
        self.invok.do_workload(opRands, ano)
        # self.chk_status()

    def lastFileNameInTest(self, tree):
            # Change here to get the actual filenum
            return self.invok.mk_file_nm(tree, self.invok.filenum - 1)

    def checkDirListEmpty(self, emptyDirList):
        for d in emptyDirList:
            if exists(d):
                assert os.listdir(d) == []

    def cleanup_files(self):
        self.runTest("cleanup",[])

    def mk_files(self):
        self.cleanup_files()
        self.runTest("create")
        lastfn = self.generated_files[-1]
        self.assertTrue(exists(lastfn))
        assert (
            os.path.getsize(lastfn)
            == self.invok.total_sz_kb * self.invok.BYTES_PER_KB
        )

    def reset_top(self,top):
        self.invok.set_top([top])
        self.invok.reset_tree(top)

ok = 0
from commands import *

def mutate_chosen_basic_program(basic_program):
    commands = get_commands()
    com_name = []
    com_weight = []
    for can_com in commands:
        com_name.append(can_com["name"])
        com_weight.append(can_com["weight"])
    for id in range(0,len(basic_program)):
        if random.random() <= 0.1: # 10% possiblity to mutate
            new_command = random.choices(com_name, weights=com_weight, k=1)[0]
            basic_program[id] = new_command
        else:
            continue
    return basic_program

def generate_cur_program(command_num, program_pool):
    # should return the current program
    # if program_pool:
    #     # we choose based on an existing program
    #     chosen_basic_program = random.choice(program_pool)
    #     res_program = mutate_chosen_basic_program(chosen_basic_program)
    #     return res_program
    
    #  if the program pool is empty
    commands = get_commands()
    res_program = []
    for i in range(0, command_num):
        # decide the i_th command in this program
        com_name = []
        com_weight = []
        for can_com in commands:
            # if the current candidate command has a pre command and the command is not in the program yet
            if (len(can_com["pre_command"]) != 0) and (can_com["pre_command"][0] not in res_program):
                continue
            com_name.append(can_com["name"])
            com_weight.append(can_com["weight"])
        # choose a command according to its weight
        # print(com_name)
        # print(com_weight)
        chosen_command = random.choices(com_name, weights=com_weight, k=1)[0]
        res_program.append(chosen_command)
    return res_program


class Execute_Program(multiprocessing.Process):
    def __init__(self,fuzzer,command,params,ano):
        multiprocessing.Process.__init__(self)
        self.fuzzer = fuzzer
        self.command = command
        self.params = params
        self.ano = ano
    def run(self):
        if self.ano == Ano.DELAY:
            time.sleep(2)
        self.fuzzer.runTest(self.command,self.params, self.ano)
        # execute done

def check_oracle():
    return check_ceph_oracle()

def record_program(cur_program):
    # TODO: record the current program to the log file for reproduce
    return

# create the concurrent commands for each client to exeucte the command concurrently
def create_horcrux(cur_command):
    if ["append","overwrite","truncate-overwrite"].__contains__(cur_command):
        candidates = ["append","overwrite","truncate-overwrite"]
        chosen_horcrux = random.choice(candidates)
        return chosen_horcrux
    elif ["rename","symlink","delete","chmod","hardlink"].__contains__(cur_command):
        candidates = ["rename","symlink","delete","chmod","hardlink"]
        chosen_horcrux = random.choice(candidates)
        return chosen_horcrux
    elif ["create", "mkdir", "rmdir"].__contains__(cur_command):
        candidates = ["create", "mkdir", "rmdir"]
        chosen_horcrux = random.choice(candidates)
        return chosen_horcrux
    return cur_command

def select_para(file_tree, cur_command):
    if ["append","delete","chmod","overwrite","truncate-overwrite"].__contains__(cur_command):
        # for these commands return an existing file
        current_files = file_tree.get_all_files()
        selected_para = [random.choice(current_files)]
        return selected_para
    elif ["rename","symlink","hardlink"].__contains__(cur_command):
        # for these commands return an existing file and a new file name
        current_files = file_tree.get_all_files()
        existing_file = random.choice(current_files)
        # generate new file name
        prefix = "Eris_"
        middle = get_random_string(10)
        suffix = str(int(time.time()))
        file_type = get_random_string(3)

        symfile_dir = random.choice(file_tree.get_all_dirs())

        new_file = symfile_dir + "/" + prefix + middle + "_" + suffix + "." + file_type
        selected_para = [existing_file, new_file]
        return selected_para
    elif ["create"].__contains__(cur_command):
        current_dirs = file_tree.get_all_dirs()
        current_dirs.append(file_tree.get_path())
        prefix_dir = random.choice(current_dirs)
        # for these commands return a new file name
        prefix = "Eris_"
        middle = get_random_string(10)
        suffix = str(int(time.time()))
        file_type = get_random_string(3)
        # Eris_dfjkaghkhe_2387471094.age
        new_file = prefix_dir + "/" + prefix + middle + "_" + suffix + "." + file_type
        return [new_file]
    elif ["mkdir"].__contains__(cur_command):
        # return a new dir name
        current_dirs = file_tree.get_all_dirs()
        current_dirs.append(file_tree.get_path())
        prefix_dir = random.choice(current_dirs)
        # generate new dir name
        prefix = "Eris_"
        middle = get_random_string(10)
        suffix = str(time.time())
        file_type = "d"
        # Eris_dfjkaghkhe_2387471094.d    
        new_dir = prefix_dir + "/" + prefix + middle + "_" + suffix + "." + file_type
        return [new_dir]
    elif ["rmdir"].__contains__(cur_command):
        # return an existing dir name
        current_dirs = file_tree.get_all_dirs()
        selected_para = random.choice(current_dirs)
        return [selected_para]
    else:
        return []


def select_para_under_guidance(file_tree, parallel_op, root_paras):
    cur_command = parallel_op.opcode
    if ["append","delete","chmod","overwrite","truncate-overwrite"].__contains__(cur_command):
        # for these commands return an existing file, one-one relation or two-one relation
        #  if the root operation has one parameter, which is the same as the parallel operation
        root_para0 = root_paras[0]
        relation = parallel_op.relation[0]
        selected_para = []
        if relation == Relation.SELF: # the selected parameter should be exactly the same as the root oepration
            selected_para.append(root_para0)
        elif relation == Relation.FATHER: # the selected parameter should be the father of the root oepration
            father = file_tree.get_father(root_para0)
            selected_para.append(father)
        elif relation == Relation.SON: # the selected parameter should be the son of the root oepration
            childs = file_tree.get_child(root_para0)
            child = random.choice(childs)
            selected_para.append(child)
        else: # the selected parameter should be noRel
            current_files = file_tree.get_all_files()
            selected_para = [random.choice(current_files)]
        print("Select relation:",relation)
        print("Root para:",root_para0)
        print("Selected para",selected_para)
        return selected_para
    
    elif ["rename","symlink","hardlink"].__contains__(cur_command):
        # for these commands return an existing file and a new file name, one-two or two-two
        root_para0 = root_paras[0]
        relation = parallel_op.relation[0]
        print("Select relation:",relation)
        selected_para = []
        if relation == Relation.SELF: # the selected parameter should be exactly the same as the root oepration
            selected_para.append(root_para0)
        elif relation == Relation.FATHER: # the selected parameter should be the father of the root oepration
            father = file_tree.get_father(root_para0)
            selected_para.append(father)
        elif relation == Relation.SON: # the selected parameter should be the son of the root oepration
            childs = file_tree.get_child(root_para0)
            child = random.choice(childs)
            selected_para.append(child)
        else: # the selected parameter should be noRel
            current_files = file_tree.get_all_files()
            selected_para = [random.choice(current_files)]
        if len(root_paras) == 1:
            #  the root operation has no second param
            # generate new file name
            prefix = "Eris_"
            middle = get_random_string(10)
            suffix = str(int(time.time()))
            file_type = get_random_string(3)

            symfile_dir = random.choice(file_tree.get_all_dirs())

            new_file = symfile_dir + "/" + prefix + middle + "_" + suffix + "." + file_type
            selected_para.append(new_file)
        else:
            root_para1 = root_paras[1]
            relation = parallel_op.relation[1]
            # selected_para = []
            if relation == Relation.SELF: # the selected parameter should be exactly the same as the root oepration
                selected_para.append(root_para1)
            elif relation == Relation.FATHER: # the selected parameter should be the father of the root oepration
                father = file_tree.get_father(root_para1)
                selected_para.append(father)
            elif relation == Relation.SON: # the selected parameter should be the son of the root oepration
                childs = file_tree.get_child(root_para1)
                child = random.choice(childs)
                selected_para.append(child)
            else: # the selected parameter should be noRel
                prefix = "Eris_"
                middle = get_random_string(10)
                suffix = str(int(time.time()))
                file_type = get_random_string(3)

                symfile_dir = random.choice(file_tree.get_all_dirs())

                new_file = symfile_dir + "/" + prefix + middle + "_" + suffix + "." + file_type
                selected_para.append(new_file)
        return selected_para
    elif ["create"].__contains__(cur_command):
        root_para0 = root_paras[0]
        relation = parallel_op.relation[0]
        print("Select relation:",relation)
        selected_para = []
        if relation == Relation.SELF: # the selected parameter should be exactly the same as the root oepration
            selected_para.append(root_para0)
        elif relation == Relation.FATHER: # the selected parameter should be the father of the root oepration
            father = file_tree.get_father(root_para0)
            selected_para.append(father)
        elif relation == Relation.SON: # the selected parameter should be the son of the root oepration
            childs = file_tree.get_child(root_para0)
            child = random.choice(childs)
            selected_para.append(child)
        else: # the selected parameter should be noRel
            current_dirs = file_tree.get_all_dirs()
            current_dirs.append(file_tree.get_path())
            prefix_dir = random.choice(current_dirs)
            # for these commands return a new file name
            prefix = "Eris_"
            middle = get_random_string(10)
            suffix = str(int(time.time()))
            file_type = get_random_string(3)
            # Eris_dfjkaghkhe_2387471094.age
            new_file = prefix_dir + "/" + prefix + middle + "_" + suffix + "." + file_type
            selected_para = [new_file]
        return selected_para
    
    elif ["mkdir"].__contains__(cur_command):
        # return a new dir name
        root_para0 = root_paras[0]
        relation = parallel_op.relation[0]
        print("Select relation:",relation)
        selected_para = []
        if relation == Relation.SELF: # the selected parameter should be exactly the same as the root oepration
            selected_para.append(root_para0)
        elif relation == Relation.FATHER: # the selected parameter should be the father of the root oepration
            father = file_tree.get_father(root_para0)
            selected_para.append(father)
        elif relation == Relation.SON: # the selected parameter should be the son of the root oepration
            childs = file_tree.get_child(root_para0)
            child = random.choice(childs)
            selected_para.append(child)
        else: # the selected parameter should be noRel
            current_dirs = file_tree.get_all_dirs()
            current_dirs.append(file_tree.get_path())
            prefix_dir = random.choice(current_dirs)
            # generate new dir name
            prefix = "Eris_"
            middle = get_random_string(10)
            suffix = str(time.time())
            file_type = "d"
            # Eris_dfjkaghkhe_2387471094.d    
            new_dir = prefix_dir + "/" + prefix + middle + "_" + suffix + "." + file_type
            selected_para = [new_dir]
        return selected_para
    elif ["rmdir"].__contains__(cur_command):
        # return an existing dir name
        root_para0 = root_paras[0]
        relation = parallel_op.relation[0]
        print("Select relation:",relation)
        selected_para = []
        if relation == Relation.SELF: # the selected parameter should be exactly the same as the root oepration
            selected_para.append(root_para0)
        elif relation == Relation.FATHER: # the selected parameter should be the father of the root oepration
            father = file_tree.get_father(root_para0)
            selected_para.append(father)
        elif relation == Relation.SON: # the selected parameter should be the son of the root oepration
            childs = file_tree.get_child(root_para0)
            child = random.choice(childs)
            selected_para.append(child)
        else: # the selected parameter should be noRel
            current_dirs = file_tree.get_all_dirs()
            selected_para.append(random.choice(current_dirs))
        return selected_para
    else:
        return []

def get_random_string(length):
    # choose from all lowercase letter
    letters = string.ascii_lowercase
    result_str = ''.join(random.choice(letters) for i in range(length))
    return result_str

def reset_top(path_str, origin_top, new_top):
    tail = path_str.split(origin_top)[1]
    ret = new_top + tail
    return ret

class MetaDataVariant:
    def __init__(self):
        self.max_delta_width = 0
        self.max_delta_depth = 0
        self.max_delta_meta = 0
        
        self.cur_width = 0
        self.cur_depth = 0
        self.cur_meta = 0
    
    def get_max_delta_width(self):
        return self.max_delta_width
    
    def get_max_delta_depth(self):
        return self.max_delta_depth

    def get_max_delta_meta(self):
        return self.max_delta_meta

    def set_max_delta_width(self, width):
        self.max_delta_width = width
    
    def set_max_delta_depth(self, depth):
        self.max_delta_depth = depth

    def set_max_delta_meta(self, meta):
        self.max_delta_meta = meta

    # setter and getter for cur values
    def get_cur_width(self):
        return self.cur_width
    
    def get_cur_depth(self):
        return self.cur_depth

    def get_cur_meta(self):
        return self.cur_meta

    def set_cur_width(self, width):
        self.cur_width = width
    
    def set_cur_depth(self, depth):
        self.cur_depth = depth

    def set_cur_meta(self, meta):
        self.cur_meta = meta

    # get the latest delta_width
    def update_delta_width(self, tree):
        tree.update_tree()
        new_width = tree.get_cur_width()
        delta_width = abs(new_width - self.cur_width)
        return delta_width

    # get the latest delta_depth
    def update_delta_depth(self, tree):
        tree.update_tree()
        new_depth = tree.get_cur_depth()
        delta_depth = abs(new_depth - self.cur_depth)
        return delta_depth

def run_fuzzing(clients_mount_places, pre_process=True):
    # how many clients are using fuzzing
    num_of_fuzzing_clients = len(clients_mount_places)
    commands_per_program = 1
    meta_data_variant = MetaDataVariant()
    file_relation_table = CommandMap()
    program_pool = []
    max_period = 0
    print("ready")
    print(clients_mount_places)
    # setup fuzzer for each client
    fuzzers = []
    for n in range(0, num_of_fuzzing_clients):
        fuzzer = Fuzzer()
        # setup the fuzzer first
        fuzzer.setUp()
        fuzzer.reset_top(clients_mount_places[n])
        if pre_process:
            fuzzer.cleanup_files()
        fuzzers.append(fuzzer)

    if pre_process:
        # Pre-processing create 200 files and 200 dirs
        print("================ Pre-Processing ===================")
        print("Phase 1: Create directories")
        for c in range(0,200):
            cur_params = select_para(fuzzers[0].invok.file_tree,"mkdir")
            fuzzers[0].runTest("mkdir",cur_params)
            print("create dir:",cur_params[0])
        print("Phase 1 Done!!!")
        print("Phase 2: Create Files")
        for c in range(0,200):
            cur_params = select_para(fuzzers[0].invok.file_tree,"create")
            fuzzers[0].runTest("create",cur_params)
            print("create file:",cur_params[0])
        print("Phase 2 Done!!!")
        # Start the fuzzing process
        print()
    print()
    print("================ Start Fuzzing ===================")
    i = 1
    while True:
        # generate a program with the setting number of commands
        cur_program = generate_cur_program(commands_per_program, program_pool)
        # fuzzer.invok.log.info("================== EXECUTE PROGRAM:%d =================",i)
        print("EXECUTE PROGRAM "+str(i)+":")
        print(cur_program)
        # start the multiple processes for each client's fuzzing
        each_command = cur_program[0] #only one command in a program
        print()
        print("*** execute %s start ***" % each_command)
        fuzzing_processes = []
        process_ano = []
        ano = random.choice(list(Ano))
        root_operation = generate_root_command(each_command,ano)
        # select the current parameter for the currnent command
        # The return value is a list where indicates the paras of the command
        cur_params = select_para(fuzzers[0].invok.file_tree,each_command)
        # start_time = time.time()
        # generate the parallel operations
        parallel_ops = file_relation_table.select_parallel_ops(root_operation,num_of_fuzzing_clients-1)
        for n in range(0, num_of_fuzzing_clients):
            if n == 0:
                p = Execute_Program(fuzzers[n],each_command, cur_params, ano)
                fuzzing_processes.append(p)
                process_ano.append(ano)
                print("Client %d executes %s %s" %(n+1, each_command, cur_params))
                # f = open('./last_file','a')
                # f.write(cur_params[0])
                # f.close()
            else:
                # new_command = create_horcrux(each_command)
                new_command = parallel_ops[n-1].opcode
                new_ano = parallel_ops[n-1].ano
                # new_params = select_para(fuzzers[n].invok.file_tree,new_command)
                new_params = select_para_under_guidance(fuzzers[n].invok.file_tree,parallel_ops[n-1],cur_params)
                # # ensure the first oprands are the same
                # new_params[0] = reset_top(cur_params[0],fuzzers[0].invok.top_dirs[0],fuzzers[n].invok.top_dirs[0])
                p = Execute_Program(fuzzers[n],new_command, new_params, new_ano)
                fuzzing_processes.append(p)
                process_ano.append(new_ano)
                print("Client %d executes %s %s" %(n+1, new_command, new_params))
        
        # record the current timestamp
        exec_start_time = time.time()

        # execute the generated program
        for n in range(0, num_of_fuzzing_clients):
            fuzzing_processes[n].start()
        
        time.sleep(2)
        # for n in range(0, num_of_fuzzing_clients):
        #     if process_ano[n] == Ano.SUSPEND:
        #         pid = fuzzing_processes[n].pid
        #         os.kill(pid, signal.SIGTSTP)
        # wait_time = random.uniform(1, 5)
        # time.sleep(wait_time)

        # for n in range(0, num_of_fuzzing_clients):
        #     if process_ano[n] == Ano.SUSPEND:
        #         pid = fuzzing_processes[n].pid
        #         os.kill(pid, signal.SIGCONT)

        for n in range(0, num_of_fuzzing_clients):
            fuzzing_processes[n].join()

        print("*** execute %s over ***" % each_command)
        print()
        print("EXECUTION SUCCESS!!!!!")
        # check oracle results
        # TODO: pass in the fuzzers into the check oracle function, to check whether each tree is equal
        res = check_oracle()
        checking_times = 1
        while not check_oracle():
            if checking_times == 10:
               print("CHECKING 10 TIMES FAILURE, BUG OCCURS!!!!!")
               record_program(cur_program)
               break
            # return
            # checking failure, a bug occurs
            # first record
            print("Checking times:",checking_times)
            print("CHECKING FAILURE, POTENTIAL BUG OCCURS!!!!! Check again after 10 seconds")
            print()
            time.sleep(10)
            checking_times = 1 + checking_times
        end_time = time.time()  
        period = end_time - exec_start_time
        f_consistency_time = open("/root/ceph_consistency_Eris.csv",'a')
        f_consistency_time.write(str(i) + ',' + str(time.time()) + ',' + str(period) + '\n')
        f_consistency_time.close()
        # TODO: check whether the current program is a good one
        i += 1

        if (max_period <= period):
            max_period = period
            file_relation_table.update(root_operation, parallel_ops)

        # delta_width = meta_data_variant.update_delta_depth(fuzzers[0].invok.file_tree)
        # delta_depth = meta_data_variant.update_delta_width(fuzzers[0].invok.file_tree) 
        # max_delta_width = meta_data_variant.get_max_delta_width()
        # max_delta_depth = meta_data_variant.get_max_delta_depth()

        # # if the current program is a good one
        # if (delta_width >= 0.8 * float(max_delta_width)) or (delta_depth >= 0.8 * float(max_delta_depth)):
        #     # if the delta width or depth is more than 80% of the maximum delta, we think the program is good
        #     # program_pool.append(cur_program)
        #     # we update the current relation table
        #     file_relation_table.update(root_operation, parallel_ops)
        # if delta_width > max_delta_width:
        #     meta_data_variant.set_max_delta_width(delta_width)
        # if delta_depth > max_delta_depth:
        #     meta_data_variant.set_max_delta_depth(delta_depth)


def run_reproduce():
    fuzzer = Fuzzer()
    # setup the fuzzer first
    fuzzer.setUp()
    # fuzzer.runTest("read")

if __name__ == "__main__":
    # Change here
    clients_mount_places = ["/mnt/mycephfsA/test_dfs_fuzzer/smf/","/mnt/mycephfsB/test_dfs_fuzzer/smf/","/mnt/mycephfsC/test_dfs_fuzzer/smf/","/mnt/mycephfsD/test_dfs_fuzzer/smf/","/mnt/mycephfsE/test_dfs_fuzzer/smf/"]
    run_fuzzing(clients_mount_places, True)
    # ret = reset_top("/mnt/mycephfs1/test_dfs_fuzzer/smf/tets/test", "/mnt/mycephfs1/test_dfs_fuzzer/smf/", "/mnt/mycephfs2/test_dfs_fuzzer/smf/")
    # print(ret)
    
