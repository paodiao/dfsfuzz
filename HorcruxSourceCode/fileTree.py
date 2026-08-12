# define current file tree in the distributed file system
# this is used in both oracles and the fuzzing guidance
# Created by fcorleone on August 7th, 2023

import json
import os
import random

class FileTree:
    def __init__(self, path):
        self.path = path
        self.update_tree()
    tree_json = ""
    cur_depth = 0
    cur_width = 0

    def update_depth_and_width(self):
        level = 1
        w = 0
        pre_items = 0
        while True:
            # loop until we get the depth
            command = "tree " + self.path + " -L " + str(level) + " -f -J -o ./tmp/length.json"
            # print(command)
            os.system(command)
            f = open('./tmp/length.json', 'r')
            cur_json = json.load(f)
            f.close()
            # print(cur_json)
            dirs = cur_json[1]['directories']
            files = cur_json[1]['files']
            cur_items = dirs + files
            if cur_items > pre_items:
                # the current level has new items
                cur_width = cur_items - pre_items
                if cur_width > w:
                    w = cur_width
                pre_items = cur_items
                level = level + 1
            else:
                # currently we have no new items found
                break
        self.cur_width = w
        self.cur_depth = level-1

    def update_tree(self):
        command = "tree " + self.path + " -f -J -o ./tmp/tree_out.json"
        os.system(command)
        # print(command)
        f = open('./tmp/tree_out.json', 'r')
        self.tree_json = json.load(f)
        f.close()
        # after update the tree, automatically update the depth and the width
        self.update_depth_and_width()

    def get_cur_width(self):
        return self.cur_width
    
    def get_cur_depth(self):
        return self.cur_depth

    def get_father(self, name):
        cur_dir_json = self.tree_json[0]
        # print(cur_dir_json['contents'])
        father = []
        cur_father = [self.path]
        self.traverse_father(name, cur_dir_json, cur_father, father)
        if len(father) == 0:
            return os.path.dirname(name)
        return father[0]

    def traverse_father(self, name, cur_dir_json, cur_father, father):
        # print(name)
        for item in cur_dir_json['contents']:
            if len(father) != 0:
                break
            if 'type' in item:
                # print(item)
                if item['name'] == name or item['name']+'/' == name:
                    # we get the father
                    father.append(cur_father[0])
                elif item['type'] == "directory":
                    cur_father.clear()
                    cur_father.append(item['name'])
                    self.traverse_father(name, item, cur_father, father)

    def traverse_child(self,name,cur_dir_json,child):
        # print(name)
        for item in cur_dir_json['contents']:
            if len(child) != 0:
                break
            if 'type' in item:
                # print(item)
                if item['name'] == name or item['name']+'/' == name:
                    # current find the target node:
                    if item['type'] == "directory":
                        # we get the children
                        children = item['contents']
                        if len(children) == 0:
                            child.append(name)
                        else:
                            child.append(random.choice(children)['name'])
                elif item['type'] == "directory":
                    self.traverse_child(name, item, child)

    def get_child(self, name):
        cur_dir_json = self.tree_json[0]
        # print(cur_dir_json['contents'])
        child = []
        self.traverse_child(name, cur_dir_json, child)
        if len(child) == 0:
            return [name]
        return child

    def get_all_files(self):
        #  return a list of file names in the current tree
        self.update_tree()
        # traverse the tree json
        cur_dir_json = self.tree_json[0]
        file_list = []
        dir_list = []
        traverse_tree_json(cur_dir_json,file_list,dir_list)
        return file_list

    def get_all_dirs(self):
        #  return a list of file names in the current tree
        self.update_tree()
        # print(self.path)
        # traverse the tree json
        cur_dir_json = self.tree_json[0]
        file_list = []
        dir_list = []
        traverse_tree_json(cur_dir_json,file_list,dir_list)
        return dir_list

    def get_path(self):
        return self.path
        


def traverse_tree_json(json_object,file_list,dir_list):
    # print(json_object)
    for item in json_object['contents']:
        if 'type' in item:
            if item['type'] == "directory":
                # current find a directory:
                dir_list.append(item['name'])
                traverse_tree_json(item,file_list,dir_list)
            else:
                # current we find a file
                file_list.append(item['name'])



# Test for tree class

if __name__ == '__main__':
    newTree = FileTree('/mnt/mycephfs1/test_dfs_fuzzer/')
    # print(newTree.get_cur_depth())
    # print(newTree.get_cur_width())
    # print(newTree.get_child('./test_dfs_fuzzer/smf/file_srcdir/d_000'))
    print(newTree.get_father('/mnt/mycephfs1/test_dfs_fuzzer/smf/Eris_snqvisfuxs_1699842185.9711735.d/Eris_ealpjdwrmy_1699842188.224857.d/Eris_uhqaqtoami_1699842189.3791847.d/Eris_zxbcdkmnxb_1699842204.051981.d/Eris_bmcoixezan_1699843887.9697275.d'))
    # print(newTree.get_all_files())