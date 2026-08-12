#!/usr/bin/env python3
import os
import sys
import random
import string
import time
import argparse


def get_random_string(length):
    letters = string.ascii_lowercase
    result_str = ''.join(random.choice(letters) for i in range(length))
    return result_str


def get_random_file_size_kb():
    sizes = [1, 2, 4, 8, 16, 32, 64, 128, 256, 512]
    weights = [20, 20, 15, 15, 10, 5, 5, 5, 3, 2]
    return random.choices(sizes, weights=weights, k=1)[0]


def generate_random_content(size_bytes):
    return os.urandom(size_bytes)


def generate_file_name(base_dir):
    prefix = "Eris_"
    middle = get_random_string(10)
    suffix = random.randint(0, 10 ** 12)
    file_type = get_random_string(3)
    file_name = os.path.join(base_dir, f"{prefix}{middle}_{suffix}.{file_type}")
    return file_name


def generate_dir_name(base_dir):
    prefix = "Eris_"
    middle = get_random_string(10)
    suffix = random.randint(0, 10 ** 12)
    dir_name = os.path.join(base_dir, f"{prefix}{middle}_{suffix}.d")
    return dir_name


def create_intermediate_dirs(base_path, target_path):
    intermediate_dirs = []
    rel_path = os.path.relpath(target_path, base_path)
    parts = rel_path.split(os.sep)
    
    current_path = base_path
    for i in range(len(parts) - 1):
        current_path = os.path.join(current_path, parts[i])
        rel_intermediate = os.path.relpath(current_path, base_path)
        if not os.path.exists(current_path):
            os.makedirs(current_path, exist_ok=True)
            intermediate_dirs.append(rel_intermediate)
    
    return intermediate_dirs


def main():
    parser = argparse.ArgumentParser(description='Generate test files and directories for multiple nodes')
    parser.add_argument('node_ids', nargs='+', help='Node ID strings')
    parser.add_argument('--num-files', type=int, default=50, help='Number of files to generate per node')
    parser.add_argument('--num-dirs', type=int, default=50, help='Number of directories to generate per node')
    parser.add_argument('--max-depth', type=int, default=8, help='Maximum directory depth')
    parser.add_argument('--num-large-files', type=int, default=1100, help='Number of files in the large directory for dcache persistence test')
    parser.add_argument('--num-empty-dirs', type=int, default=100, help='Number of empty directories to generate per node')
    parser.add_argument('--seed', type=int, default=None, help='Random seed for reproducible generation')
    args = parser.parse_args()
    
    if args.seed is not None:
        random.seed(args.seed)
    
    node_ids = args.node_ids
    num_files = args.num_files
    num_dirs = args.num_dirs
    max_depth = args.max_depth
    num_large_files = args.num_large_files
    num_empty_dirs = args.num_empty_dirs
    
    script_dir = os.path.dirname(os.path.abspath(__file__))
    
    global_target_files = set()
    global_target_dirs = set()
    
    large_dir_info = {}
    empty_dirs_info = {}
    
    large_dir_node_idx = random.randint(0, len(node_ids) - 1)
    large_dir_node_id = node_ids[large_dir_node_idx]
    
    for node_idx, node_id in enumerate(node_ids):
        print(f"Processing node: {node_id}")
        
        node_dir = os.path.join(script_dir, node_id)
        os.makedirs(node_dir, exist_ok=True)
        
        target_files = []
        target_file_sizes = []
        target_dirs = []
        intermediate_dirs = []
        empty_dirs = []
        
        current_dirs = ["."]
        
        for i in range(num_dirs):
            attempts = 0
            max_attempts = 100
            # 15% of the directories are forced deep to guarantee coverage
            # of the deep depth buckets (5+ levels below merge_view/).
            force_deep = i < int(num_dirs * 0.15)
            
            while attempts < max_attempts:
                if force_deep:
                    # 从"深度 ≥4 且可深入"的目录池选父——深层目录分散在不同
                    # 深度层，形成多分支深层树（而非单链）；深度 4 的父 → 新
                    # 目录深度 5（merge_view 后 5 个组件 → '/' 数 ≥5 → bucket 3）
                    deep_pool = [d for d in current_dirs
                                 if 4 <= d.count(os.sep) < max_depth]
                    if not deep_pool:
                        # 还没有深度 ≥4 的目录（前期）→ 从最深的可深入池选，先建到深层
                        max_d = max(d.count(os.sep) for d in current_dirs)
                        deep_pool = [d for d in current_dirs
                                     if d.count(os.sep) == max_d and max_d < max_depth]
                    if deep_pool:
                        parent_dir = random.choice(deep_pool)
                    else:
                        parent_dir = random.choice(current_dirs)
                elif random.random() < 0.3:
                    # 强制深入：从还能深入的目录池选父
                    can_deepen = [d for d in current_dirs if d.count(os.sep) < max_depth]
                    if can_deepen:
                        parent_dir = random.choice(can_deepen)
                    else:
                        parent_dir = random.choice(current_dirs)
                else:
                    parent_dir = random.choice(current_dirs)
                
                if parent_dir == ".":
                    parent_path = node_dir
                else:
                    parent_path = os.path.join(node_dir, parent_dir)
                
                new_dir_rel = generate_dir_name(parent_dir) if parent_dir != "." else generate_dir_name(".")
                new_dir_rel = os.path.relpath(new_dir_rel, ".")
                
                if new_dir_rel not in global_target_dirs:
                    global_target_dirs.add(new_dir_rel)
                    
                    new_dir_full = os.path.join(node_dir, new_dir_rel)
                    
                    intermediate = create_intermediate_dirs(node_dir, new_dir_full)
                    for inter in intermediate:
                        if inter not in intermediate_dirs:
                            intermediate_dirs.append(inter)
                    
                    os.makedirs(new_dir_full, exist_ok=True)
                    target_dirs.append(new_dir_rel)
                    
                    current_dirs.append(new_dir_rel)
                    break
                
                attempts += 1
                time.sleep(0.000001)
            
            if attempts >= max_attempts:
                print(f"  Warning: Could not generate unique directory after {max_attempts} attempts")
        
        for i in range(num_empty_dirs):
            attempts = 0
            max_attempts = 100
            
            while attempts < max_attempts:
                parent_dir = random.choice(current_dirs)
                
                if parent_dir == ".":
                    parent_path = node_dir
                else:
                    parent_path = os.path.join(node_dir, parent_dir)
                
                empty_dir_rel = generate_dir_name(parent_dir) if parent_dir != "." else generate_dir_name(".")
                empty_dir_rel = os.path.relpath(empty_dir_rel, ".")
                
                if empty_dir_rel not in global_target_dirs:
                    global_target_dirs.add(empty_dir_rel)
                    
                    empty_dir_full = os.path.join(node_dir, empty_dir_rel)
                    
                    intermediate = create_intermediate_dirs(node_dir, empty_dir_full)
                    for inter in intermediate:
                        if inter not in intermediate_dirs:
                            intermediate_dirs.append(inter)
                    
                    os.makedirs(empty_dir_full, exist_ok=True)
                    empty_dirs.append(empty_dir_rel)
                    # NOTE: empty dirs must NOT join current_dirs — later file
                    # generation would fill them and they would no longer be empty.
                    break
                
                attempts += 1
                time.sleep(0.000001)
            
            if attempts >= max_attempts:
                print(f"  Warning: Could not generate unique empty directory after {max_attempts} attempts")
        
        large_dir_files = []
        large_dir_file_sizes = []
        is_large_dir_node = (node_id == large_dir_node_id)
        
        if is_large_dir_node:
            print(f"  Creating large directory for dcache persistence test...")
            
            attempts = 0
            max_attempts = 100
            large_dir_rel = None
            
            while attempts < max_attempts:
                parent_dir = random.choice(current_dirs)
                
                if parent_dir == ".":
                    parent_path = node_dir
                else:
                    parent_path = os.path.join(node_dir, parent_dir)
                
                large_dir_rel = generate_dir_name(parent_dir) if parent_dir != "." else generate_dir_name(".")
                large_dir_rel = os.path.relpath(large_dir_rel, ".")
                
                if large_dir_rel not in global_target_dirs:
                    global_target_dirs.add(large_dir_rel)
                    
                    large_dir_full = os.path.join(node_dir, large_dir_rel)
                    
                    intermediate = create_intermediate_dirs(node_dir, large_dir_full)
                    for inter in intermediate:
                        if inter not in intermediate_dirs:
                            intermediate_dirs.append(inter)
                    
                    os.makedirs(large_dir_full, exist_ok=True)
                    
                    current_dirs.append(large_dir_rel)
                    break
                
                attempts += 1
                time.sleep(0.000001)
            
            if large_dir_rel:
                large_dir_full = os.path.join(node_dir, large_dir_rel)
                
                for i in range(num_large_files):
                    new_file_rel = generate_file_name(large_dir_rel)
                    new_file_rel = os.path.relpath(new_file_rel, ".")
                    
                    if new_file_rel not in global_target_files:
                        global_target_files.add(new_file_rel)
                        
                        new_file_full = os.path.join(node_dir, new_file_rel)
                        
                        file_size_kb = 1
                        file_size_bytes = file_size_kb * 1024
                        content = generate_random_content(file_size_bytes)
                        
                        with open(new_file_full, 'wb') as f:
                            f.write(content)
                        
                        large_dir_files.append(new_file_rel)
                        large_dir_file_sizes.append(file_size_bytes)
                    
                    if i % 100 == 0:
                        time.sleep(0.000001)
                
                large_dir_info = {
                    'node_id': node_id,
                    'dir_path': large_dir_rel,
                    'files': large_dir_files,
                    'file_sizes': large_dir_file_sizes
                }
                
                print(f"  Created large directory with {len(large_dir_files)} files")
        
        for i in range(num_files):
            attempts = 0
            max_attempts = 100
            
            while attempts < max_attempts:
                parent_dir = random.choice(current_dirs)
                
                if parent_dir == ".":
                    parent_path = node_dir
                else:
                    parent_path = os.path.join(node_dir, parent_dir)
                
                new_file_rel = generate_file_name(parent_dir) if parent_dir != "." else generate_file_name(".")
                new_file_rel = os.path.relpath(new_file_rel, ".")
                
                if new_file_rel not in global_target_files:
                    global_target_files.add(new_file_rel)
                    
                    new_file_full = os.path.join(node_dir, new_file_rel)
                    
                    intermediate = create_intermediate_dirs(node_dir, new_file_full)
                    for inter in intermediate:
                        if inter not in intermediate_dirs:
                            intermediate_dirs.append(inter)
                    
                    file_size_kb = get_random_file_size_kb()
                    file_size_bytes = file_size_kb * 1024
                    content = generate_random_content(file_size_bytes)
                    
                    with open(new_file_full, 'wb') as f:
                        f.write(content)
                    
                    target_files.append(new_file_rel)
                    target_file_sizes.append(file_size_bytes)
                    break
                
                attempts += 1
                time.sleep(0.000001)
            
            if attempts >= max_attempts:
                print(f"  Warning: Could not generate unique file after {max_attempts} attempts")
        
        if empty_dirs:
            empty_dirs_info[node_id] = empty_dirs
        
        file_list_path = os.path.join(script_dir, f"{node_id}.file")
        with open(file_list_path, 'w') as f:
            for idx, file_path in enumerate(target_files):
                f.write(f"{file_path} {target_file_sizes[idx]}\n")
        
        dir_list_path = os.path.join(script_dir, f"{node_id}.dir")
        with open(dir_list_path, 'w') as f:
            for dir_path in target_dirs:
                f.write(dir_path + '\n')
        
        tmpdir_list_path = os.path.join(script_dir, f"{node_id}.tmpdir")
        with open(tmpdir_list_path, 'w') as f:
            for dir_path in intermediate_dirs:
                f.write(dir_path + '\n')
        
        print(f"  Created {len(target_files)} files, {len(target_dirs)} directories, {len(intermediate_dirs)} intermediate directories, {len(empty_dirs)} empty directories")
    
    if large_dir_info:
        large_dir_path = os.path.join(script_dir, "large_dir.info")
        with open(large_dir_path, 'w') as f:
            f.write(f"node_id: {large_dir_info['node_id']}\n")
            f.write(f"dir_path: {large_dir_info['dir_path']}\n")
            f.write(f"file_count: {len(large_dir_info['files'])}\n")
            f.write("files:\n")
            for idx, file_path in enumerate(large_dir_info['files']):
                f.write(f"  {file_path} {large_dir_info['file_sizes'][idx]}\n")
        print(f"\nLarge directory info written to: large_dir.info")
    
    if empty_dirs_info:
        empty_dirs_path = os.path.join(script_dir, "empty_dirs.info")
        with open(empty_dirs_path, 'w') as f:
            for node_id, dirs in empty_dirs_info.items():
                f.write(f"node_id: {node_id}\n")
                for dir_path in dirs:
                    f.write(f"  {dir_path}\n")
        print(f"Empty directories info written to: empty_dirs.info")
    
    print("\nGeneration complete!")
    print(f"Total unique target files: {len(global_target_files)}")
    print(f"Total unique target directories: {len(global_target_dirs)}")
    if large_dir_info:
        print(f"Large directory for dcache persistence: {large_dir_info['dir_path']} ({len(large_dir_info['files'])} files) on node {large_dir_info['node_id']}")
    if empty_dirs_info:
        total_empty = sum(len(dirs) for dirs in empty_dirs_info.values())
        print(f"Total empty directories: {total_empty}")


if __name__ == "__main__":
    main()
