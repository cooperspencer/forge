package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Package types.
const (
	PkgContainer = "container"
	PkgGeneric   = "generic"
)

// ErrExists: the thing being created already exists.
var ErrExists = errors.New("already exists")

type Package struct {
	ID        int64
	OwnerID   int64
	Owner     string
	Type      string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (p *Package) FullName() string { return p.Owner + "/" + p.Name }

const pkgSelect = `SELECT p.id, p.owner_id, u.name, p.type, p.name, p.created_at, p.updated_at FROM packages p JOIN users u ON u.id=p.owner_id`

func scanPackage(sc interface{ Scan(...any) error }) (*Package, error) {
	var p Package
	var c, u int64
	if err := sc.Scan(&p.ID, &p.OwnerID, &p.Owner, &p.Type, &p.Name, &c, &u); err != nil {
		return nil, notFound(err)
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return &p, nil
}

func (s *Store) queryPackages(ctx context.Context, q string, args ...any) ([]*Package, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Package
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PackageByName(ctx context.Context, ownerID int64, typ, name string) (*Package, error) {
	return scanPackage(s.db.QueryRowContext(ctx, pkgSelect+` WHERE p.owner_id=? AND p.type=? AND p.name=?`, ownerID, typ, name))
}

func (s *Store) PackageByID(ctx context.Context, id int64) (*Package, error) {
	return scanPackage(s.db.QueryRowContext(ctx, pkgSelect+` WHERE p.id=?`, id))
}

// EnsurePackage returns the package, creating it on first use.
func (s *Store) EnsurePackage(ctx context.Context, ownerID int64, typ, name string) (*Package, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO packages(owner_id,type,name,created_at,updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(owner_id,type,name) DO NOTHING`, ownerID, typ, name, now(), now())
	if err != nil {
		return nil, err
	}
	return s.PackageByName(ctx, ownerID, typ, name)
}

// Packages lists packages, all of them for ownerID 0.
func (s *Store) Packages(ctx context.Context, ownerID int64) ([]*Package, error) {
	if ownerID == 0 {
		return s.queryPackages(ctx, pkgSelect+` ORDER BY u.name, p.name, p.type`)
	}
	return s.queryPackages(ctx, pkgSelect+` WHERE p.owner_id=? ORDER BY p.name, p.type`, ownerID)
}

func (s *Store) TouchPackage(ctx context.Context, id int64) {
	s.db.ExecContext(ctx, `UPDATE packages SET updated_at=? WHERE id=?`, now(), id)
}

func (s *Store) DeletePackage(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM packages WHERE id=?`, id)
	return err
}

// ------------------------------------------------------------------ blobs

func (s *Store) PutBlob(ctx context.Context, digest string, size int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pkg_blobs(digest,size,created_at) VALUES (?,?,?) ON CONFLICT(digest) DO NOTHING`, digest, size, now())
	return err
}

func (s *Store) BlobSize(ctx context.Context, digest string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT size FROM pkg_blobs WHERE digest=?`, digest).Scan(&n)
	return n, notFound(err)
}

// ------------------------------------------------------------------ OCI

func (s *Store) LinkBlob(ctx context.Context, pkgID int64, digest string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oci_blob_links(package_id,digest,created_at) VALUES (?,?,?)
		ON CONFLICT(package_id,digest) DO UPDATE SET created_at=excluded.created_at`, pkgID, digest, now())
	return err
}

func (s *Store) BlobLinked(ctx context.Context, pkgID int64, digest string) bool {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM oci_blob_links WHERE package_id=? AND digest=?`, pkgID, digest).Scan(&one) == nil
}

type Upload struct {
	ID        string
	PackageID int64
	Size      int64
	CreatedAt time.Time
}

func (s *Store) CreateUpload(ctx context.Context, id string, pkgID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oci_uploads(id,package_id,created_at) VALUES (?,?,?)`, id, pkgID, now())
	return err
}

func (s *Store) UploadByID(ctx context.Context, id string) (*Upload, error) {
	var u Upload
	var c int64
	err := s.db.QueryRowContext(ctx, `SELECT id, package_id, size, created_at FROM oci_uploads WHERE id=?`, id).Scan(&u.ID, &u.PackageID, &u.Size, &c)
	if err != nil {
		return nil, notFound(err)
	}
	u.CreatedAt = time.Unix(c, 0)
	return &u, nil
}

func (s *Store) SetUploadSize(ctx context.Context, id string, size int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oci_uploads SET size=? WHERE id=?`, size, id)
	return err
}

func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oci_uploads WHERE id=?`, id)
	return err
}

type Manifest struct {
	PackageID int64
	Digest    string
	MediaType string
	Size      int64
	Refs      []string
	CreatedAt time.Time
}

func (s *Store) PutManifest(ctx context.Context, m *Manifest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oci_manifests(package_id,digest,media_type,size,refs,created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(package_id,digest) DO NOTHING`, m.PackageID, m.Digest, m.MediaType, m.Size, strings.Join(m.Refs, " "), now())
	if err == nil {
		s.TouchPackage(ctx, m.PackageID)
	}
	return err
}

func (s *Store) ManifestByDigest(ctx context.Context, pkgID int64, digest string) (*Manifest, error) {
	var m Manifest
	var refs string
	var c int64
	err := s.db.QueryRowContext(ctx, `SELECT package_id, digest, media_type, size, refs, created_at FROM oci_manifests WHERE package_id=? AND digest=?`,
		pkgID, digest).Scan(&m.PackageID, &m.Digest, &m.MediaType, &m.Size, &refs, &c)
	if err != nil {
		return nil, notFound(err)
	}
	m.Refs, m.CreatedAt = strings.Fields(refs), time.Unix(c, 0)
	return &m, nil
}

func (s *Store) Manifests(ctx context.Context, pkgID int64) ([]*Manifest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT package_id, digest, media_type, size, refs, created_at FROM oci_manifests WHERE package_id=? ORDER BY created_at DESC`, pkgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Manifest
	for rows.Next() {
		var m Manifest
		var refs string
		var c int64
		if err := rows.Scan(&m.PackageID, &m.Digest, &m.MediaType, &m.Size, &refs, &c); err != nil {
			return nil, err
		}
		m.Refs, m.CreatedAt = strings.Fields(refs), time.Unix(c, 0)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// DeleteManifest removes a manifest and every tag pointing at it.
func (s *Store) DeleteManifest(ctx context.Context, pkgID int64, digest string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM oci_manifests WHERE package_id=? AND digest=?`, pkgID, digest)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oci_tags WHERE package_id=? AND digest=?`, pkgID, digest); err != nil {
		return err
	}
	return tx.Commit()
}

type Tag struct {
	Name      string
	Digest    string
	UpdatedAt time.Time
}

func (s *Store) SetTag(ctx context.Context, pkgID int64, tag, digest string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oci_tags(package_id,tag,digest,updated_at) VALUES (?,?,?,?)
		ON CONFLICT(package_id,tag) DO UPDATE SET digest=excluded.digest, updated_at=excluded.updated_at`, pkgID, tag, digest, now())
	if err == nil {
		s.TouchPackage(ctx, pkgID)
	}
	return err
}

func (s *Store) TagDigest(ctx context.Context, pkgID int64, tag string) (string, error) {
	var d string
	err := s.db.QueryRowContext(ctx, `SELECT digest FROM oci_tags WHERE package_id=? AND tag=?`, pkgID, tag).Scan(&d)
	return d, notFound(err)
}

// Tags lists tags, most recently updated first.
func (s *Store) Tags(ctx context.Context, pkgID int64) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tag, digest, updated_at FROM oci_tags WHERE package_id=? ORDER BY updated_at DESC, tag`, pkgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		var u int64
		if err := rows.Scan(&t.Name, &t.Digest, &u); err != nil {
			return nil, err
		}
		t.UpdatedAt = time.Unix(u, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTag(ctx context.Context, pkgID int64, tag string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oci_tags WHERE package_id=? AND tag=?`, pkgID, tag)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------------ generic

type PkgFile struct {
	PackageID int64
	Version   string
	Filename  string
	Digest    string
	Size      int64
	Creator   string
	CreatedAt time.Time
}

func (s *Store) AddFile(ctx context.Context, f *PkgFile) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pkg_files(package_id,version,filename,digest,size,creator,created_at) VALUES (?,?,?,?,?,?,?)`,
		f.PackageID, f.Version, f.Filename, f.Digest, f.Size, f.Creator, now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrExists
	}
	if err == nil {
		s.TouchPackage(ctx, f.PackageID)
	}
	return err
}

const fileCols = `package_id, version, filename, digest, size, creator, created_at`

func scanFile(sc interface{ Scan(...any) error }) (*PkgFile, error) {
	var f PkgFile
	var c int64
	if err := sc.Scan(&f.PackageID, &f.Version, &f.Filename, &f.Digest, &f.Size, &f.Creator, &c); err != nil {
		return nil, notFound(err)
	}
	f.CreatedAt = time.Unix(c, 0)
	return &f, nil
}

func (s *Store) File(ctx context.Context, pkgID int64, version, filename string) (*PkgFile, error) {
	return scanFile(s.db.QueryRowContext(ctx, `SELECT `+fileCols+` FROM pkg_files WHERE package_id=? AND version=? AND filename=?`, pkgID, version, filename))
}

// Files lists a package's files, newest version first.
func (s *Store) Files(ctx context.Context, pkgID int64) ([]*PkgFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+fileCols+` FROM pkg_files WHERE package_id=? ORDER BY created_at DESC, version, filename`, pkgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PkgFile
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteFiles deletes one file, or every file of the version if filename is "".
func (s *Store) DeleteFiles(ctx context.Context, pkgID int64, version, filename string) error {
	var res sql.Result
	var err error
	if filename == "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM pkg_files WHERE package_id=? AND version=?`, pkgID, version)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM pkg_files WHERE package_id=? AND version=? AND filename=?`, pkgID, version, filename)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------------ GC

// GCState is what garbage collection needs to decide which blobs are live.
type GCState struct {
	Referenced map[string]bool // by files, manifests or manifest references
	Links      []struct {
		PackageID int64
		Digest    string
		CreatedAt time.Time
	}
	Blobs []struct {
		Digest    string
		CreatedAt time.Time
	}
}

func (s *Store) LoadGCState(ctx context.Context) (*GCState, error) {
	st := &GCState{Referenced: map[string]bool{}}
	rows, err := s.db.QueryContext(ctx, `SELECT digest, '' FROM pkg_files UNION ALL SELECT digest, refs FROM oci_manifests`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d, refs string
		if err := rows.Scan(&d, &refs); err != nil {
			rows.Close()
			return nil, err
		}
		st.Referenced[d] = true
		for _, r := range strings.Fields(refs) {
			st.Referenced[r] = true
		}
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT package_id, digest, created_at FROM oci_blob_links`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l struct {
			PackageID int64
			Digest    string
			CreatedAt time.Time
		}
		var c int64
		if err := rows.Scan(&l.PackageID, &l.Digest, &c); err != nil {
			rows.Close()
			return nil, err
		}
		l.CreatedAt = time.Unix(c, 0)
		st.Links = append(st.Links, l)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT digest, created_at FROM pkg_blobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b struct {
			Digest    string
			CreatedAt time.Time
		}
		var c int64
		if err := rows.Scan(&b.Digest, &c); err != nil {
			return nil, err
		}
		b.CreatedAt = time.Unix(c, 0)
		st.Blobs = append(st.Blobs, b)
	}
	return st, rows.Err()
}

func (s *Store) UnlinkBlob(ctx context.Context, pkgID int64, digest string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oci_blob_links WHERE package_id=? AND digest=?`, pkgID, digest)
	return err
}

func (s *Store) DeleteBlobRow(ctx context.Context, digest string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pkg_blobs WHERE digest=?`, digest)
	return err
}

// StaleUploads returns ids of uploads started before t.
func (s *Store) StaleUploads(ctx context.Context, t time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM oci_uploads WHERE created_at < ?`, t.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UploadExists reports whether an upload session row exists (for orphan file cleanup).
func (s *Store) UploadExists(ctx context.Context, id string) bool {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM oci_uploads WHERE id=?`, id).Scan(&one) == nil
}

// DeleteEmptyPackages removes packages with no content left that were last
// touched before t.
func (s *Store) DeleteEmptyPackages(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM packages WHERE updated_at < ?
		AND NOT EXISTS (SELECT 1 FROM pkg_files f WHERE f.package_id=packages.id)
		AND NOT EXISTS (SELECT 1 FROM oci_manifests m WHERE m.package_id=packages.id)
		AND NOT EXISTS (SELECT 1 FROM oci_blob_links l WHERE l.package_id=packages.id)
		AND NOT EXISTS (SELECT 1 FROM oci_uploads u WHERE u.package_id=packages.id)`, t.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
