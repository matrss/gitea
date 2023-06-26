package annex

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"code.gitea.io/gitea/modules/git"
)

func repoArchiveTargz(repo *git.Repository, target io.Writer, prefix, commitID string) error {
	// create a plain git tar archive
	var tarB bytes.Buffer
	err := repo.CreateArchive(repo.Ctx, git.TAR, &tarB, prefix != "", commitID)
	if err != nil {
		return err
	}

	gitFilesR := tar.NewReader(&tarB)

	gzipW := gzip.NewWriter(target)
	defer gzipW.Close()
	tarW := tar.NewWriter(gzipW)
	defer tarW.Close()

	tree, err := repo.GetTree(commitID)
	if err != nil {
		return err
	}

	for {
		oldHeader, err := gitFilesR.Next()
		// TODO: handle non-local names in tar archives?
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		// default to copying the current file from the archive created by repo.CreateArchive
		header := oldHeader
		dataR := io.Reader(gitFilesR)

		// if we can get a annex content location for the file we use that instead
		blob, err := tree.GetBlobByPath(strings.TrimPrefix(oldHeader.Name, prefix))
		if err == nil {
			annexPath, err := ContentLocation(blob)
			if err == nil {
				// blob corresponds to an annexed file

				// build a tar header for the annexed file
				file, err := os.Open(annexPath)
				if err != nil {
					return fmt.Errorf("opening %s failed: %w", annexPath, err)
				}
				stat, err := file.Stat()
				if err != nil {
					return fmt.Errorf("getting FileInfo for %s failed: %w", file.Name(), err)
				}
				header, err = tar.FileInfoHeader(stat, "")
				if err != nil {
					return fmt.Errorf("creating header failed: %w", err)
				}
				header.Name = prefix + blob.Name()

				// set the data reader
				dataR = file
			}
		}

		// write header
		err = tarW.WriteHeader(header)
		if err != nil {
			return fmt.Errorf("writing header for %s failed: %w", header.Name, err)
		}

		// write data
		_, err = io.Copy(tarW, dataR)
		if err != nil {
			return fmt.Errorf("writing data for %s failed: %w", header.Name, err)
		}
	}

	return nil
}

func repoArchiveZip(repo *git.Repository, target io.Writer, prefix, commitID string) error {
	// create a plain git zip archive
	var zipB bytes.Buffer
	err := repo.CreateArchive(repo.Ctx, git.ZIP, &zipB, prefix != "", commitID)
	if err != nil {
		return err
	}

	gitFilesR, err := zip.NewReader(bytes.NewReader(zipB.Bytes()), int64(zipB.Len()))
	if err != nil {
		return err
	}

	tree, err := repo.GetTree(commitID)
	if err != nil {
		return err
	}

	zipW := zip.NewWriter(target)
	defer zipW.Close()

	err = zipW.SetComment(gitFilesR.Comment)
	if err != nil {
		return fmt.Errorf("setting archive comment field failed: %w", err)
	}

	for _, f := range gitFilesR.File {
		oldHeader := f.FileHeader

		// default to copying the current file from the archive created by repo.CreateArchive
		// dataR is set later to avoid unnecessarily opening a file here
		header := &oldHeader
		dataR := io.Reader(nil)

		blob, err := tree.GetBlobByPath(strings.TrimPrefix(oldHeader.Name, prefix))
		if err == nil {
			annexPath, err := ContentLocation(blob)
			if err == nil {
				// blob corresponds to an annexed file

				// build a zip header for the file
				file, err := os.Open(annexPath)
				if err != nil {
					return fmt.Errorf("opening %s failed: %w", annexPath, err)
				}
				stat, err := file.Stat()
				if err != nil {
					return fmt.Errorf("getting FileInfo for %s failed: %w", file.Name(), err)
				}
				header, err = zip.FileInfoHeader(stat)
				if err != nil {
					return fmt.Errorf("creating header failed: %w", err)
				}
				header.Name = prefix + blob.Name()
				header.Method = zip.Deflate

				// set the data reader
				dataR = file
			}
		}

		if dataR == nil {
			// data reader was not yet set, take the data from the archive created by repo.CreateArchive
			file, err := f.Open()
			if err != nil {
				return fmt.Errorf("opening %s failed: %w", f.Name, err)
			}
			dataR = file
		}

		// write header
		fileW, err := zipW.CreateHeader(header)
		if err != nil {
			return fmt.Errorf("writing header for %s failed: %w", header.Name, err)
		}

		// write data
		_, err = io.Copy(fileW, dataR)
		if err != nil {
			return fmt.Errorf("writing data for %s failed: %w", header.Name, err)
		}
	}

	return nil
}

// RepoArchive creates an archive of format from repo at commitID and writes it to target.
// Files in the archive are prefixed with the repositories name if usePrefix is true.
// It is an annex-aware alternative to Repository.CreateArchive in the git package.
func RepoArchive(repo *git.Repository, format git.ArchiveType, target io.Writer, usePrefix bool, commitID string) error {
	if format.String() == "unknown" {
		return fmt.Errorf("unknown format: %v", format)
	}

	var prefix string
	if usePrefix {
		prefix = filepath.Base(strings.TrimSuffix(repo.Path, ".git")) + "/"
	} else {
		prefix = ""
	}

	var err error
	if format == git.TARGZ {
		err = repoArchiveTargz(repo, target, prefix, commitID)
	} else if format == git.ZIP {
		err = repoArchiveZip(repo, target, prefix, commitID)
	} else {
		return fmt.Errorf("unsupported format: %v", format)
	}
	if err != nil {
		return fmt.Errorf("failed to create archive: %w", err)
	}

	return nil
}
