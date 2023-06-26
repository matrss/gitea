// Copyright 2015 The Gogs Authors. All rights reserved.
// Copyright 2020 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ArchiveType archive types
type ArchiveType int

const (
	// ZIP zip archive type
	ZIP ArchiveType = iota + 1
	// TARGZ tar gz archive type
	TARGZ
	// BUNDLE bundle archive type
	BUNDLE
)

// String converts an ArchiveType to string
func (a ArchiveType) String() string {
	switch a {
	case ZIP:
		return "zip"
	case TARGZ:
		return "tar.gz"
	case BUNDLE:
		return "bundle"
	}
	return "unknown"
}

func ToArchiveType(s string) ArchiveType {
	switch s {
	case "zip":
		return ZIP
	case "tar.gz":
		return TARGZ
	case "bundle":
		return BUNDLE
	}
	return 0
}

// annexedFiles returns all files in the annex which are present in gitea.
func (repo *Repository) annexedFiles(ctx context.Context, commitID string) ([]string, error) {
	// annex_cmd := NewCommand(ctx, "annex", "find", "--anything", "--print0")
	cmd := NewCommand(ctx, "annex", "find", "--print0")
	cmd.AddOptionFormat("--branch=%s", commitID)

	var stderr strings.Builder
	var stdout strings.Builder
	err := cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return nil, ConcatenateError(err, stderr.String())
	}
	annexedFiles := strings.Split(stdout.String(), "\x00")
	annexedFiles = annexedFiles[:len(annexedFiles)-1]

	return annexedFiles, nil
}

func isIn(l []string, s string) bool {
	for _, e := range l {
		if e == s {
			return true
		}
	}
	return false
}

// getAnnexKeyForFile returns the annex key for a file in the repo at commitID.
func (repo *Repository) getAnnexKeyForFile(ctx context.Context, commitID string, file string) (string, error) {
	// Get the key of the annexed file.
	// I would prefer to use git annex lookupkey here, but that does not work on bare repositories.
	// TODO: properly handle git annex pointer files, this works for basic ones though
	cmd := NewCommand(ctx, "show")
	cmd.AddDynamicArguments(fmt.Sprintf("%s:%s", commitID, file))
	var stderr strings.Builder
	var stdout strings.Builder
	err := cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return "", ConcatenateError(err, stderr.String())
	}
	tmp := strings.Split(stdout.String(), "/")
	annex_key := strings.TrimSuffix(tmp[len(tmp)-1], "\n")
	return annex_key, nil
}

// getAnnexContentLocation returns the full path to an annexed file identified by its key.
func (repo *Repository) getAnnexContentLocation(ctx context.Context, key string) (string, error) {
	// Get the full path to the annexed file.
	cmd := NewCommand(ctx, "annex", "contentlocation")
	cmd.AddDynamicArguments(key)
	var stderr strings.Builder
	var stdout strings.Builder
	err := cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return "", ConcatenateError(err, stderr.String())
	}
	annex_path := stdout.String()
	annex_path = strings.TrimSuffix(annex_path, "\n")
	return repo.Path + "/" + annex_path, nil
}

// createGitArchive uses git archive to generate a archive of the supplied format and writes it to target.
func (repo *Repository) createGitArchive(ctx context.Context, format string, target io.Writer, prefix string, commitID string) error {
	cmd := NewCommand(ctx, "archive")
	if prefix != "" {
		cmd.AddOptionFormat("--prefix=%s", prefix)
	}
	cmd.AddOptionFormat("--format=%s", format)
	cmd.AddDynamicArguments(commitID)

	var stderr strings.Builder
	err := cmd.Run(&RunOpts{
		Dir:    repo.Path,
		Stdout: target,
		Stderr: &stderr,
	})
	if err != nil {
		return ConcatenateError(err, stderr.String())
	}

	return nil
}

// createArchiveTargz creates a tar.gz archive containing the content of repo, including annexed files, and writes it to target.
func (repo *Repository) createArchiveTargz(ctx context.Context, target io.Writer, prefix string, commitID string, annexedFiles []string) error {
	rd, w := io.Pipe()
	defer func() {
		rd.Close()
		w.Close()
	}()

	done := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("%v", r)
			}
		}()

		err := repo.createGitArchive(ctx, "tar", w, prefix, commitID)
		_ = w.CloseWithError(err)
		done <- err
	}()

	gitFilesR := tar.NewReader(rd)

	gzipW := gzip.NewWriter(target)
	defer gzipW.Close()
	tarW := tar.NewWriter(gzipW)
	defer tarW.Close()

	for {
		header, err := gitFilesR.Next()
		// TODO: handle non-local names in tar archives?
		if err != nil {
			if err == io.EOF {
				// Finish reading until the end of the pipe, otherwise the go routine running git archive will be stuck
				// This essentially finishes reading the end-of-archive entry (two 512 byte blocks of zero bytes)
				_, err := io.ReadAll(rd)
				if err != nil {
					return err
				}
				break
			}
			return err
		}

		// Do not write files from git which are also annexed (i.e. all the files that are tracked by git-annex)
		if isIn(annexedFiles, strings.TrimPrefix(header.Name, prefix)) {
			continue
		}

		err = tarW.WriteHeader(header)
		if err != nil {
			return err
		}

		_, err = io.Copy(tarW, gitFilesR)
		if err != nil {
			return err
		}
	}

	for _, annexedFile := range annexedFiles {
		annexKey, err := repo.getAnnexKeyForFile(ctx, commitID, annexedFile)
		if err != nil {
			return err
		}

		annexPath, err := repo.getAnnexContentLocation(ctx, annexKey)
		if err != nil {
			return err
		}

		file, err := os.Open(annexPath)
		if err != nil {
			return err
		}
		stat, err := file.Stat()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(stat, "")
		if err != nil {
			return err
		}
		header.Name = prefix + annexedFile
		err = tarW.WriteHeader(header)
		if err != nil {
			return err
		}
		_, err = io.Copy(tarW, file)
		if err != nil {
			return err
		}
	}

	err := <-done
	if err != nil {
		return err
	}

	return nil
}

// createArchiveZip creates a zip archive containing the content of repo, including annexed files, and writes it to target.
func (repo *Repository) createArchiveZip(ctx context.Context, target io.Writer, prefix string, commitID string, annexedFiles []string) error {
	rd, w := io.Pipe()
	defer func() {
		rd.Close()
		w.Close()
	}()

	done := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("%v", r)
			}
		}()

		err := repo.createGitArchive(ctx, "zip", w, prefix, commitID)
		_ = w.CloseWithError(err)
		done <- err
	}()

	zipBuffer := bytes.NewBuffer([]byte{})
	nb, err := io.Copy(zipBuffer, rd)
	if err != nil {
		return err
	}
	gitFilesR, err := zip.NewReader(bytes.NewReader(zipBuffer.Bytes()), nb)
	if err != nil {
		return err
	}

	zipW := zip.NewWriter(target)
	defer zipW.Close()

	zipW.SetComment(gitFilesR.Comment)

	for _, f := range gitFilesR.File {
		header := f.FileHeader

		// Do not write files from git which are also annexed (i.e. all the files that are tracked by git-annex)
		if isIn(annexedFiles, strings.TrimPrefix(header.Name, prefix)) {
			continue
		}

		// TODO: I am not sure if reusing the header instead of creating a copy is safe
		fileW, err := zipW.CreateHeader(&header)
		if err != nil {
			return err
		}

		file, err := f.Open()
		if err != nil {
			return err
		}

		_, err = io.Copy(fileW, file)
		if err != nil {
			return err
		}
	}
	for _, annexedFile := range annexedFiles {
		annexKey, err := repo.getAnnexKeyForFile(ctx, commitID, annexedFile)
		if err != nil {
			return err
		}

		annexPath, err := repo.getAnnexContentLocation(ctx, annexKey)
		if err != nil {
			return err
		}

		file, err := os.Open(annexPath)
		if err != nil {
			return err
		}
		stat, err := file.Stat()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(stat)
		if err != nil {
			return err
		}
		header.Name = prefix + annexedFile
		header.Method = zip.Deflate
		fileW, err := zipW.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = io.Copy(fileW, file)
		if err != nil {
			return err
		}
	}

	err = <-done
	if err != nil {
		return err
	}

	return nil
}

// CreateArchive create archive content to the target path
func (repo *Repository) CreateArchive(ctx context.Context, format ArchiveType, target io.Writer, usePrefix bool, commitID string) error {
	if format.String() == "unknown" {
		return fmt.Errorf("unknown format: %v", format)
	}

	var prefix string
	if usePrefix {
		prefix = filepath.Base(strings.TrimSuffix(repo.Path, ".git")) + "/"
	} else {
		prefix = ""
	}

	annexedFiles, err := repo.annexedFiles(ctx, commitID)
	if err != nil {
		return err
	}

	if format == TARGZ {
		err := repo.createArchiveTargz(ctx, target, prefix, commitID, annexedFiles)
		if err != nil {
			return err
		}
	} else if format == ZIP {
		err := repo.createArchiveZip(ctx, target, prefix, commitID, annexedFiles)
		if err != nil {
			return err
		}
	} else if format == BUNDLE {
		return fmt.Errorf("unsupported format: %v", format)
	}

	return nil
}
