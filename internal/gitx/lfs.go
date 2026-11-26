package gitx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// LFSPointer is a parsed Git LFS pointer file.
type LFSPointer struct {
	OID  string // sha256 hex
	Size int64
}

// MaxLFSPointerSize: pointer files are tiny; anything bigger is real content.
const MaxLFSPointerSize = 1024

// ParseLFSPointer recognises the content of a Git LFS pointer file.
func ParseLFSPointer(b []byte) (LFSPointer, bool) {
	var p LFSPointer
	if len(b) > MaxLFSPointerSize || !bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec/")) {
		return p, false
	}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch k {
		case "oid":
			p.OID = strings.TrimPrefix(v, "sha256:")
		case "size":
			p.Size, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return p, ValidLFSOID(p.OID) && p.Size >= 0
}

// ValidLFSOID: 64 lowercase hex characters.
func ValidLFSOID(oid string) bool {
	if len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// LFSPointers lists the LFS objects referenced by blobs reachable from rev but
// not from any of not (e.g. the commits a merge brings in).
func (r *Repo) LFSPointers(ctx context.Context, rev string, not ...string) ([]LFSPointer, error) {
	if err := safe(append([]string{rev}, not...)...); err != nil {
		return nil, err
	}
	args := []string{"rev-list", "--objects", "--no-object-names", rev}
	if len(not) > 0 {
		args = append(append(args, "--not"), not...)
	}
	objs, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(objs)) == 0 {
		return nil, nil
	}
	// keep the small blobs: only those can be pointers
	info, err := r.run(ctx, objs, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	var small bytes.Buffer
	for _, l := range strings.Split(string(info), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[1] == "blob" {
			if n, _ := strconv.Atoi(f[2]); n <= MaxLFSPointerSize && n > 0 {
				small.WriteString(f[0] + "\n")
			}
		}
	}
	if small.Len() == 0 {
		return nil, nil
	}
	out, err := r.run(ctx, small.Bytes(), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	var ptrs []LFSPointer
	seen := map[string]bool{}
	br := bufio.NewReader(bytes.NewReader(out))
	for {
		hdr, err := br.ReadString('\n')
		if err != nil {
			break
		}
		var sha, typ string
		var size int
		if _, err := fmt.Sscanf(hdr, "%s %s %d", &sha, &typ, &size); err != nil {
			break
		}
		body := make([]byte, size+1) // content plus the trailing newline
		if _, err := io.ReadFull(br, body); err != nil {
			break
		}
		if p, ok := ParseLFSPointer(body[:size]); ok && !seen[p.OID] {
			seen[p.OID] = true
			ptrs = append(ptrs, p)
		}
	}
	return ptrs, nil
}
