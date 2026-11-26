package gitx

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
)

// Jujutsu (jj) writes its change ID into the commits it creates, as an extra
// header "change-id <32 letters k-z>". A change keeps its ID when jj rewrites
// it (amend, rebase, squash), so the ID links the versions of a change.

// ShortChangeID is how jj shows change IDs by default.
func ShortChangeID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// fillChangeIDs reads the change-id headers of cs (one git cat-file call).
func (r *Repo) fillChangeIDs(ctx context.Context, cs []Commit) {
	if len(cs) == 0 {
		return
	}
	var in bytes.Buffer
	for _, c := range cs {
		in.WriteString(c.SHA + "\n")
	}
	ids, err := r.changeIDs(ctx, in.Bytes())
	if err != nil {
		return
	}
	for i := range cs {
		cs[i].ChangeID = ids[cs[i].SHA]
	}
}

func (r *Repo) changeIDs(ctx context.Context, shas []byte) (map[string]string, error) {
	out, err := r.run(ctx, shas, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	br := bufio.NewReader(bytes.NewReader(out))
	for {
		hdr, err := br.ReadString('\n')
		if err != nil {
			break
		}
		f := strings.Fields(hdr) // <sha> <type> <size> | <sha> missing
		if len(f) != 3 {
			continue
		}
		size, _ := strconv.Atoi(f[2])
		body := make([]byte, size+1) // content + trailing newline
		if _, err := io.ReadFull(br, body); err != nil {
			break
		}
		if f[1] != "commit" {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			if line == "" {
				break // end of headers
			}
			if v, ok := strings.CutPrefix(line, "change-id "); ok {
				ids[f[0]] = strings.TrimSpace(v)
			}
		}
	}
	return ids, nil
}

// PatchID identifies a commit's changes independent of its parent and
// message (git patch-id --stable); "" for merges and empty commits.
func (r *Repo) PatchID(ctx context.Context, sha string) string {
	if safe(sha) != nil {
		return ""
	}
	patch, err := r.run(ctx, nil, "diff-tree", "-p", "--no-color", "--root", "--no-ext-diff", "--end-of-options", sha)
	if err != nil || len(bytes.TrimSpace(patch)) == 0 {
		return ""
	}
	out, err := r.run(ctx, patch, "patch-id", "--stable")
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// MergeTrees three-way merges ours and theirs over base (any tree-ish) into
// a new tree, without touching refs or a work tree.
func (r *Repo) MergeTrees(ctx context.Context, base, ours, theirs string) (tree string, conflicts []string, err error) {
	if err := safe(base, ours, theirs); err != nil {
		return "", nil, err
	}
	return r.runMergeTree(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", "--merge-base="+base, ours, theirs)
}
