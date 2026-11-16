package prog

type DFSConfig struct {
	Profile                 *DFSProfile
	DfsName                 string
	Cids                    []string
	Node_num                int // seed-generation slots; GlusterFS sets this to ClientNum
	Serv_num                int // leading empty server programs in an executor batch
	ClientNum               int // client programs in the executor batch
	TotalNum                int // full batch size: ServNum + ClientNum
	RootProgIdx             int // absolute index of the first client (= ServNum)
	RootPrefix              string
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

type Hmdfs_config = DFSConfig
