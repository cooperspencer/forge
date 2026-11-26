package gitx

import "testing"

func TestParseLFSPointer(t *testing.T) {
	oid := "4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393"
	p, ok := ParseLFSPointer([]byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 12345\n"))
	if !ok || p.OID != oid || p.Size != 12345 {
		t.Errorf("valid pointer: %v %+v", ok, p)
	}
	for _, bad := range []string{
		"",
		"hello world\n",
		"version https://git-lfs.github.com/spec/v1\noid sha256:xyz\nsize 1\n",              // bad oid
		"version https://git-lfs.github.com/spec/v1\noid sha256:" + oid[:60] + "\nsize 1\n", // short oid
		"version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize -5\n",     // negative size
		"oid sha256:" + oid + "\nsize 1\nversion https://git-lfs.github.com/spec/v1\n",      // not a pointer header
	} {
		if _, ok := ParseLFSPointer([]byte(bad)); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
