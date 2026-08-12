# Horcrux: Fuzzing Distributed File Systems

Horcrux is a distributed file system fuzzing tools target on metadata inconsistency bug detection. 

By generating huge amount of cross-node file operations, Horcrux makes confict conditions for metadata updating and lead to inconsistency.

### Usage and File Structure

The python files in the repository are source files of Horcrux. The `commands_map.py` and the `commands.py` are used to model the file operations. The `fileTree.py` defines functions of the tree operations in the file structure. The file `oracle.py` defines the metadata inconsistency oracles. 

It's easy to use Horcrux for fuzzing a DFS. Just modify the mount point in `dfsFuzzer.py` and simply run 

```
python3 dfsFuzzer.py
```

in the client's node. We will open-source the file `dfsFuzzer.py` later.

###  Fuzzing Phase

There are two phase in Horcrux. In the first phase, Horcrux randomly generates 200 files and 200 directories.

In the second phase, Horcrux performs the synchronization-duration-guided fuzzing.

