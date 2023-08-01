package integration

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"testing"

	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/modules/annex"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/setting"

	"github.com/stretchr/testify/require"
)

func TestGitAnnexArchive(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		ctx := NewAPITestContext(t, "user2", "annex-archive-test", auth_model.AccessTokenScopeWriteRepository)
		require.NoError(t, doCreateRemoteAnnexRepository(t, u, ctx, false))
		req := NewRequest(t, "GET", u.String())
		_ = ctx.Session.MakeRequest(t, req, http.StatusOK)

		// cleanup previously generated archives
		adminSession := loginUser(t, "user1")
		adminToken := getTokenForLoggedInUser(t, adminSession, auth_model.AccessTokenScopeWriteAdmin)
		link, _ := url.Parse("/api/v1/admin/cron/delete_repo_archives")
		link.RawQuery = url.Values{"token": {adminToken}}.Encode()
		resp := adminSession.MakeRequest(t, NewRequest(t, "POST", link.String()), http.StatusNoContent)
		bs, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Len(t, bs, 0)

		remoteRepoPath := path.Join(setting.RepoRootPath, ctx.GitPath())

		// get the commitID of the master branch
		repo, err := git.OpenRepository(git.DefaultContext, remoteRepoPath)
		require.NoError(t, err)
		commitID, err := repo.GetBranchCommitID("master")
		require.NoError(t, err)
		tree, err := repo.GetTree("master")
		require.NoError(t, err)
		entries, err := tree.ListEntriesRecursiveFast()
		require.NoError(t, err)
		filesInGit := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				filesInGit = append(filesInGit, entry.Name())
			}
		}

		t.Run("TARGZ", func(t *testing.T) {
			// request a tar.gz archive of the repo
			link, _ := url.Parse(fmt.Sprintf("/api/v1/repos/%s/%s/archive/master.tar.gz", ctx.Username, ctx.Reponame))
			resp := ctx.Session.MakeRequest(t, NewRequest(t, "GET", link.String()), http.StatusOK)
			bs, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			// open the archive for reading
			gzrd, err := gzip.NewReader(bytes.NewReader(bs))
			require.NoError(t, err)
			defer gzrd.Close()
			rd := tar.NewReader(gzrd)

			var filesInArchive []string
			for {
				header, err := rd.Next()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)

				// skip directories
				if header.Typeflag == tar.TypeDir {
					continue
				}

				// check that the pax_global_header comment field is correctly set
				if path.Base(header.Name) == "pax_global_header" {
					require.Equal(t, commitID, header.PAXRecords["comment"])
					continue // skip the remaining checks since this file does not exist in git
				}

				name := strings.TrimPrefix(header.Name, ctx.Reponame+"/")
				filesInArchive = append(filesInArchive, name)

				blob, err := tree.GetBlobByPath(name)
				require.NoError(t, err)
				isAnnexed, err := annex.IsAnnexed(blob)
				require.NoError(t, err)

				// make sure all files are the same as in the repo itself
				actualContent, err := io.ReadAll(rd)
				require.NoError(t, err)
				if isAnnexed {
					fa, err := annex.Content(blob)
					require.NoError(t, err)
					defer fa.Close()

					// check that the file mode (type and permissions) is equal
					actualFileMode := header.FileInfo().Mode()
					stat, err := fa.Stat()
					require.NoError(t, err)
					expectedFileMode := stat.Mode()
					require.Equal(t, expectedFileMode, actualFileMode)

					// check that the content is equal
					expectedContent, err := io.ReadAll(fa)
					require.NoError(t, err)
					require.Equal(t, expectedContent, actualContent)
				} else {
					// check that the content is equal
					r, err := blob.DataAsync()
					require.NoError(t, err)
					defer r.Close()
					expectedContent, err := io.ReadAll(r)
					require.NoError(t, err)
					require.Equal(t, expectedContent, actualContent)
				}
			}
			// check that all files that are in git are also present in the archive
			sort.Strings(filesInGit)
			sort.Strings(filesInArchive)
			require.Equal(t, filesInGit, filesInArchive)
		})

		t.Run("ZIP", func(t *testing.T) {
			// request a zip archive of the repo
			link, _ := url.Parse(fmt.Sprintf("/api/v1/repos/%s/%s/archive/master.zip", ctx.Username, ctx.Reponame))
			resp := ctx.Session.MakeRequest(t, NewRequest(t, "GET", link.String()), http.StatusOK)
			bs, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			// open the archive for reading
			r, err := zip.NewReader(bytes.NewReader(bs), int64(len(bs)))
			require.NoError(t, err)

			// check that the comment field is correctly set
			require.Equal(t, commitID, r.Comment)

			var filesInArchive []string
			for _, f := range r.File {
				// skip directories
				if f.FileInfo().IsDir() {
					continue
				}

				name := strings.TrimPrefix(f.Name, ctx.Reponame+"/")
				filesInArchive = append(filesInArchive, name)

				blob, err := tree.GetBlobByPath(name)
				require.NoError(t, err)
				isAnnexed, err := annex.IsAnnexed(blob)
				require.NoError(t, err)

				// make sure all files are the same as in the repo itself
				frd, err := f.Open()
				require.NoError(t, err)
				defer frd.Close()
				actualContent, err := io.ReadAll(frd)
				require.NoError(t, err)
				if isAnnexed {
					fa, err := annex.Content(blob)
					require.NoError(t, err)
					defer fa.Close()

					// check that the file mode (type and permissions) is equal
					actualFileMode := f.Mode()
					stat, err := fa.Stat()
					require.NoError(t, err)
					expectedFileMode := stat.Mode()
					require.Equal(t, expectedFileMode, actualFileMode)

					// check that the content is equal
					expectedContent, err := io.ReadAll(fa)
					require.NoError(t, err)
					require.Equal(t, expectedContent, actualContent)
				} else {
					// check that the content is equal
					r, err := blob.DataAsync()
					require.NoError(t, err)
					defer r.Close()
					expectedContent, err := io.ReadAll(r)
					require.NoError(t, err)
					require.Equal(t, expectedContent, actualContent)
				}
			}
			// check that all files that are in git are also present in the archive
			sort.Strings(filesInGit)
			sort.Strings(filesInArchive)
			require.Equal(t, filesInGit, filesInArchive)
		})
	})
}
