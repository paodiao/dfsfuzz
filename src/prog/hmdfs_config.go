package prog

type Hmdfs_config struct {
	DfsName                 string
	Cids                    []string
	Node_num                int
	Serv_num                int
	InitIp                  string
	Init_file               map[string][]string
	Init_dir                map[string][]string
	Init_tmpdir             map[string][]string
	Init_empty_dir          map[string][]string
	Node_idx_of_persistence int
	Persistence_dir         string
	File_in_persistence_dir map[string][]string
	FileSize                map[string]map[string]uint64
	FileTree                *FileTree
}
