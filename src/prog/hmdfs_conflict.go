package prog

import "strings"

const (
	conflictFileSuffix = "_conflict_dev"
	conflictDirSuffix  = "_remote_directory"
)

func CanonicalHmdfsName(path string) string {
	if path == "" {
		return path
	}
	parts := strings.Split(path, "/")
	changed := false
	for i, part := range parts {
		canon := canonicalHmdfsComponent(part)
		if canon != part {
			parts[i] = canon
			changed = true
		}
	}
	if !changed {
		return path
	}
	return strings.Join(parts, "/")
}

func canonicalHmdfsComponent(base string) string {
	if base == "" {
		return base
	}
	if strings.HasSuffix(base, conflictDirSuffix) {
		if orig := strings.TrimSuffix(base, conflictDirSuffix); orig != "" {
			return orig
		}
		return base
	}
	dot := strings.LastIndex(base, ".")
	digitEnd := dot
	if digitEnd < 0 {
		digitEnd = len(base)
	}
	i := digitEnd - 1
	for i >= 0 && base[i] >= '0' && base[i] <= '9' {
		i--
	}
	if i == digitEnd-1 {
		return base
	}
	digitStart := i + 1
	suffixStart := digitStart - len(conflictFileSuffix)
	if suffixStart < 0 || base[suffixStart:digitStart] != conflictFileSuffix {
		return base
	}
	orig := base[:suffixStart]
	if orig == "" {
		return base
	}
	return orig + base[digitEnd:]
}

func CanonicalizeFsMd(fsMd map[string]FileMetadata) map[string]FileMetadata {
	if len(fsMd) == 0 {
		return fsMd
	}
	type backup struct {
		dev  int
		name string
		md   FileMetadata
	}
	out := make(map[string]FileMetadata, len(fsMd))
	backups := make(map[string]backup)
	for name, md := range fsMd {
		canon := CanonicalHmdfsName(name)
		if canon == name {
			out[canon] = md
			continue
		}
		dev := conflictDevID(name)
		if old, ok := backups[canon]; !ok || dev < old.dev || (dev == old.dev && name < old.name) {
			backups[canon] = backup{dev: dev, name: name, md: md}
		}
	}
	for canon, fb := range backups {
		if _, ok := out[canon]; !ok {
			out[canon] = fb.md
		}
	}
	return out
}

func conflictDevID(path string) int {
	slash := strings.LastIndex(path, "/")
	base := path[slash+1:]
	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		dot = len(base)
	}
	i := dot - 1
	for i >= 0 && base[i] >= '0' && base[i] <= '9' {
		i--
	}
	if i == dot-1 {
		return -1
	}
	digitStart := i + 1
	suffixStart := digitStart - len(conflictFileSuffix)
	if suffixStart < 0 || base[suffixStart:digitStart] != conflictFileSuffix {
		return -1
	}
	n := 0
	for _, c := range base[digitStart:dot] {
		n = n*10 + int(c-'0')
	}
	return n
}
