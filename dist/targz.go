// Copyright 2026 The golang.design Initiative Authors.
// All rights reserved. Use of this source code is governed by
// a BSD-style license that can be found in the LICENSE file.

package dist

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"
)

// The three fields that make an archive of the same tree differ between two
// runs, fixed here rather than inherited from the file system.
//
// specs/053-determinism.md is a rule about the compiler and a distribution is
// not the exception: a tarball whose checksum moves cannot be verified against
// a published one, and the difference is never in the bytes anyone cares about.
//
// epoch is the modification time every entry carries. It is 1970 rather than
// the zero time because the zero time is outside the range a ustar header can
// hold, which makes the writer emit a PAX record for it and the result depend
// on which Go release wrote the tarball.
//
// dirMode, exeMode and fileMode replace the modes on disk. A source file
// checked out with a different umask would otherwise change the tarball.
var epoch = time.Unix(0, 0).UTC()

const (
	dirMode  = 0o755
	exeMode  = 0o755
	fileMode = 0o644
)

// A TarEntry is one file to write into the tarball.
type TarEntry struct {
	// Name is the path inside the tarball, with forward slashes and including
	// the top level directory.
	Name string
	// Source is the file on disk to read the bytes from.
	Source string
	// Exec reports whether the entry is executable.
	Exec bool
}

// WriteTarGz writes a gzipped tar of the tree at root, under the given prefix.
//
// The entries are sorted by name, so the tarball does not depend on the order
// the file system reports a directory in. Every directory on the way to a file
// is written before it, once, so the archive unpacks with tar's -p and without
// relying on the reader to create parents.
func WriteTarGz(w io.Writer, root, prefix string) error {
	entries, err := treeEntries(root, prefix)
	if err != nil {
		return err
	}
	return writeTarGzEntries(w, entries)
}

// treeEntries lists the files under root as tarball entries, sorted by name.
func treeEntries(root, prefix string) ([]TarEntry, error) {
	var out []TarEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// A symlink or a device in a distribution is a bug in whatever
			// built the tree, not something to copy along quietly.
			return fmt.Errorf("%s is not a regular file, and a distribution holds only regular files", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, TarEntry{
			Name:   path.Join(prefix, filepath.ToSlash(rel)),
			Source: p,
			Exec:   info.Mode().Perm()&0o100 != 0,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// writeTarGzEntries writes the entries and the directories they imply.
func writeTarGzEntries(w io.Writer, entries []TarEntry) error {
	// The level is pinned. gzip's default has changed between releases before,
	// and a tarball whose bytes depend on the Go release that packed it is not
	// reproducible in the sense specs/053-determinism.md means.
	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	// The gzip header carries a name, a modification time and an OS byte, and
	// all three leak. The OS byte in particular has moved between Go releases.
	zw.Header = gzip.Header{OS: 255}

	tw := tar.NewWriter(zw)
	seen := make(map[string]bool)
	for _, e := range entries {
		if err := writeParents(tw, seen, path.Dir(e.Name)); err != nil {
			return err
		}
		if err := writeFile(tw, e); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// writeParents writes the directory entries above dir, outermost first.
//
// seen is a map and this function produces output, which
// specs/053-determinism.md would ordinarily forbid. It is not ranged over: it
// is only ever asked about one key, and the order the entries come out in is
// the sorted order of the caller's loop.
func writeParents(tw *tar.Writer, seen map[string]bool, dir string) error {
	if dir == "." || dir == "/" || dir == "" || seen[dir] {
		return nil
	}
	if err := writeParents(tw, seen, path.Dir(dir)); err != nil {
		return err
	}
	seen[dir] = true
	return tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     dir + "/",
		Mode:     dirMode,
		ModTime:  epoch,
		Format:   tar.FormatPAX,
	})
}

func writeFile(tw *tar.Writer, e TarEntry) error {
	f, err := os.Open(e.Source)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	mode := int64(fileMode)
	if e.Exec {
		mode = exeMode
	}
	// PAX and not the writer's choice. A standard library path is long enough
	// to need an extended header, and letting the writer pick the format makes
	// the tarball depend on the longest path in the tree.
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     e.Name,
		Mode:     mode,
		Size:     info.Size(),
		ModTime:  epoch,
		Format:   tar.FormatPAX,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
