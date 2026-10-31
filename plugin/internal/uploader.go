// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lukechampine.com/blake3"

	"github.com/pkg/errors"
)

// Cloudflare Pages Direct Upload REST client. Mirrors wrangler's upload
// flow byte-for-byte, so the `simplecontainer/github-actions` Alpine
// image doesn't need node + npm + wrangler added to it.
//
// Flow — four CF-API calls to upload, one more to create the deployment:
//
//  1. GET  /accounts/{a}/pages/projects/{p}/upload-token         (CF_API_TOKEN)
//     → short-lived JWT scoped to this project's asset namespace.
//  2. POST /pages/assets/check-missing                           (JWT)
//     → body {"hashes": [...all]} returns {"result": [...missing]}.
//  3. POST /pages/assets/upload                                  (JWT)
//     → body [{"key": hash, "value": base64(content), "metadata": {"contentType"}, "base64": true}, ...]
//     Batched; wrangler uses ~40 MiB raw per batch at concurrency 3.
//  4. POST /pages/assets/upsert-hashes                           (JWT)
//     → body {"hashes": [...all]} finalises the manifest side.
//  5. POST /accounts/{a}/pages/projects/{p}/deployments          (CF_API_TOKEN)
//     → multipart/form-data: `manifest` = JSON {"/<path>": <hash>}, +
//     optional branch, commit_message, commit_hash, commit_dirty.
//
// Hash: BLAKE3(base64(content) || extension-without-leading-dot) → hex →
// first 32 chars. Verified against wrangler's `hashFile` at
// `packages/deploy-helpers/src/deploy/helpers/hash.ts`. Changing this
// collides every pre-uploaded asset (Pages rejects the upsert).
//
// Reference: cloudflare/workers-sdk `packages/wrangler/src/pages/upload.ts`
// + `packages/wrangler/src/api/pages/deploy.ts`. The endpoints are
// undocumented — if Cloudflare moves them, this file fails loudly and
// the fallback is `wrangler pages deploy`.

const (
	cfAPIBase = "https://api.cloudflare.com/client/v4"

	// Keep bucket size well under wrangler's 40 MiB to stay inside the
	// alpine deploy image's memory ceiling + leave headroom for the
	// base64 inflation (×1.33). 20 MiB raw → ~27 MiB on the wire per batch.
	maxBucketSize      = 20 * 1024 * 1024
	maxBucketFileCount = 5000

	uploadConcurrency = 3
	uploadAttempts    = 5
)

type pagesUploader struct {
	apiToken  string
	accountID string
	// baseURL is the Cloudflare API root. Defaults to `cfAPIBase` in
	// production; tests override it with an httptest.Server URL so the
	// four-call flow is exercised without touching the live account.
	baseURL string
	client  *http.Client
}

func newPagesUploader(apiToken, accountID string) *pagesUploader {
	base := cfAPIBase
	// Test hook: `SC_CLOUDFLARE_API_BASE` points at an httptest.Server
	// in URN-contract tests so the ApplyT upload exercises the four-call
	// flow without touching the live account. Deliberately prefixed
	// `SC_` so it is distinct from wrangler's own CLOUDFLARE_* env vars.
	if override := os.Getenv("SC_CLOUDFLARE_API_BASE"); override != "" {
		base = override
	}
	return &pagesUploader{
		apiToken:  apiToken,
		accountID: accountID,
		baseURL:   base,
		client:    &http.Client{Timeout: 2 * time.Minute},
	}
}

type pagesFile struct {
	// relPath is forward-slash-prefixed so wrangler's manifest shape
	// matches ("/path/in/bundle"). Pages routes requests through this.
	relPath     string
	absPath     string
	hash        string
	contentType string
	size        int64
}

// cfEnvelope is the common Cloudflare API response envelope.
// `Result` is kept raw so each caller can unmarshal its own shape.
type cfEnvelope struct {
	Success bool              `json:"success"`
	Errors  []json.RawMessage `json:"errors,omitempty"`
	Result  json.RawMessage   `json:"result"`
}

// uploadTokenResult is the shape under `.result` for upload-token.
type uploadTokenResult struct {
	JWT string `json:"jwt"`
}

// deploymentResult is the shape under `.result` for the deployment POST.
// Only the fields callers consume are listed; CF returns far more.
type deploymentResult struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// scanBundle walks `dir` recursively, hashing each regular file + detecting
// its content-type. Symlinks and hidden files are followed/included — same
// behaviour as wrangler's Pages uploader.
func scanBundle(dir string) ([]pagesFile, error) {
	var files []pagesFile
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		h, err := hashFile(path)
		if err != nil {
			return errors.Wrapf(err, "hash %q", relPath)
		}
		ct := contentTypeOf(relPath)
		files = append(files, pagesFile{
			// Forward slashes on the wire, regardless of host OS.
			relPath:     "/" + filepath.ToSlash(relPath),
			absPath:     path,
			hash:        h,
			contentType: ct,
			size:        info.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// hashFile computes wrangler's Pages file hash: BLAKE3 of base64(content) ||
// extension (dot stripped), hex-encoded, first 32 chars. The extension
// part is what disambiguates e.g. `index.html` vs an identical PDF — the
// upload keys are not just content, they are content + MIME-hint.
func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	ext := filepath.Ext(path)
	if strings.HasPrefix(ext, ".") {
		ext = ext[1:]
	}
	sum := blake3.Sum256([]byte(b64 + ext))
	return hex.EncodeToString(sum[:])[:32], nil
}

// contentTypeOf picks a MIME type from the file's extension, falling back
// to the generic application/octet-stream when Go's stdlib has no mapping.
// wrangler uses the npm `mime` package which has roughly the same table
// as Go's `mime` stdlib + user-agent/platform overrides.
func contentTypeOf(relPath string) string {
	ext := filepath.Ext(relPath)
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// doJSON issues a JSON HTTP request against the CF API with the given
// bearer token, retrying transient errors with exponential backoff.
// `out` is unmarshalled from the `.result` field of the envelope.
func (u *pagesUploader) doJSON(ctx context.Context, method, url, bearer string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return errors.Wrap(err, "marshal request body")
		}
		reqBody = bytes.NewReader(b)
	}
	var lastErr error
	for attempt := 1; attempt <= uploadAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return errors.Wrap(err, "build request")
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := u.client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(backoffDelay(attempt))
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(backoffDelay(attempt))
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = errors.Errorf("CF API %s %s: %d %s", method, url, resp.StatusCode, truncate(string(respBody), 200))
			time.Sleep(backoffDelay(attempt))
			continue
		}
		if resp.StatusCode >= 400 {
			return errors.Errorf("CF API %s %s: %d %s", method, url, resp.StatusCode, truncate(string(respBody), 1000))
		}
		if out == nil {
			return nil
		}
		var env cfEnvelope
		if err := json.Unmarshal(respBody, &env); err != nil {
			return errors.Wrapf(err, "decode envelope from %s %s", method, url)
		}
		if !env.Success {
			return errors.Errorf("CF API %s %s returned success=false: %s", method, url, env.Errors)
		}
		if len(env.Result) == 0 {
			return nil
		}
		return json.Unmarshal(env.Result, out)
	}
	return errors.Wrapf(lastErr, "CF API %s %s failed after %d attempts", method, url, uploadAttempts)
}

func backoffDelay(attempt int) time.Duration {
	// 1s, 2s, 4s, 8s, 16s — matches wrangler's `Math.pow(2, attempts) * 1000`.
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// getUploadToken fetches a short-lived JWT scoped to this project.
func (u *pagesUploader) getUploadToken(ctx context.Context, project string) (string, error) {
	url := fmt.Sprintf("%s/accounts/%s/pages/projects/%s/upload-token", u.baseURL, u.accountID, project)
	var res uploadTokenResult
	if err := u.doJSON(ctx, http.MethodGet, url, u.apiToken, nil, &res); err != nil {
		return "", err
	}
	if res.JWT == "" {
		return "", errors.New("upload-token response had empty jwt")
	}
	return res.JWT, nil
}

// checkMissing posts the full hash list and gets back the subset Pages
// does not have cached yet.
func (u *pagesUploader) checkMissing(ctx context.Context, jwt string, hashes []string) ([]string, error) {
	url := u.baseURL + "/pages/assets/check-missing"
	var missing []string
	if err := u.doJSON(ctx, http.MethodPost, url, jwt, map[string]any{"hashes": hashes}, &missing); err != nil {
		return nil, err
	}
	return missing, nil
}

// uploadPayload is one element of the body wrangler sends to /pages/assets/upload.
type uploadPayload struct {
	Key      string            `json:"key"`
	Value    string            `json:"value"`
	Base64   bool              `json:"base64"`
	Metadata map[string]string `json:"metadata"`
}

// uploadAssets uploads every file in `files` to Pages. The method batches
// requests under maxBucketSize bytes raw (not base64-encoded) per batch, so
// a 60 MiB bundle fires three POSTs serially. Wrangler uses up to three
// concurrent uploads with a PQueue — we go serial for simplicity; a 20 MiB
// bundle in two batches is already fast enough for the smoke.
func (u *pagesUploader) uploadAssets(ctx context.Context, jwt string, files []pagesFile) error {
	url := u.baseURL + "/pages/assets/upload"
	var (
		batch     []uploadPayload
		batchSize int64
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := u.doJSON(ctx, http.MethodPost, url, jwt, batch, nil); err != nil {
			return err
		}
		batch = nil
		batchSize = 0
		return nil
	}
	for _, f := range files {
		data, err := os.ReadFile(f.absPath)
		if err != nil {
			return errors.Wrapf(err, "read %q", f.relPath)
		}
		payload := uploadPayload{
			Key:      f.hash,
			Value:    base64.StdEncoding.EncodeToString(data),
			Base64:   true,
			Metadata: map[string]string{"contentType": f.contentType},
		}
		// Flush before adding if this file would push us over the bucket
		// limit. A single file larger than the limit still goes in its
		// own batch.
		if len(batch) > 0 && (batchSize+f.size > maxBucketSize || len(batch) >= maxBucketFileCount) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, payload)
		batchSize += f.size
	}
	return flush()
}

// upsertHashes finalises the manifest side by sending the full hash list
// — Pages uses this to associate the uploaded assets with the project
// before the deployment is created.
func (u *pagesUploader) upsertHashes(ctx context.Context, jwt string, hashes []string) error {
	url := u.baseURL + "/pages/assets/upsert-hashes"
	return u.doJSON(ctx, http.MethodPost, url, jwt, map[string]any{"hashes": hashes}, nil)
}

// createDeployment posts the final multipart/form-data to the deployments
// endpoint. `branch` + the commit_* fields are optional but included when
// non-empty; CF uses them as deployment bookkeeping only (no git pull).
// Returns the deployment id and its live URL.
func (u *pagesUploader) createDeployment(ctx context.Context, project, branch, commitMessage, commitHash string, manifest map[string]string) (*deploymentResult, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, errors.Wrap(err, "marshal manifest")
	}
	if err := w.WriteField("manifest", string(manifestJSON)); err != nil {
		return nil, err
	}
	if branch != "" {
		if err := w.WriteField("branch", branch); err != nil {
			return nil, err
		}
	}
	if commitMessage != "" {
		// wrangler truncates commit messages to 384 bytes.
		if len(commitMessage) > 384 {
			commitMessage = commitMessage[:384]
		}
		if err := w.WriteField("commit_message", commitMessage); err != nil {
			return nil, err
		}
	}
	if commitHash != "" {
		if err := w.WriteField("commit_hash", commitHash); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/accounts/%s/pages/projects/%s/deployments", u.baseURL, u.accountID, project)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.apiToken)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "POST deployment")
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, errors.Errorf("CF deployment POST: %d %s", resp.StatusCode, truncate(string(respBody), 1000))
	}
	var env cfEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, errors.Wrapf(err, "decode deployment response: %s", truncate(string(respBody), 500))
	}
	if !env.Success {
		return nil, errors.Errorf("CF deployment POST returned success=false: %s", env.Errors)
	}
	var out deploymentResult
	if err := json.Unmarshal(env.Result, &out); err != nil {
		return nil, errors.Wrap(err, "decode deployment result")
	}
	return &out, nil
}

// uploadBundle orchestrates the full five-call flow: scan → token →
// check-missing → upload → upsert → create deployment. Returns the
// deployment id + the pages.dev URL. Called from StaticWebsite's
// ApplyT closure after the PagesProject has been created.
type uploadLogger interface {
	Info(ctx context.Context, msg string, args ...any)
}

func (u *pagesUploader) uploadBundle(ctx context.Context, project, bundleDir, branch, commitMessage, commitHash string, log uploadLogger) (*deploymentResult, error) {
	log.Info(ctx, "cloudflare pages: scanning bundle %q", bundleDir)
	files, err := scanBundle(bundleDir)
	if err != nil {
		return nil, errors.Wrap(err, "scan bundle")
	}
	if len(files) == 0 {
		return nil, errors.Errorf("bundle %q contains no files", bundleDir)
	}
	log.Info(ctx, "cloudflare pages: %d files scanned", len(files))

	allHashes := make([]string, 0, len(files))
	byHash := make(map[string]pagesFile, len(files))
	for _, f := range files {
		allHashes = append(allHashes, f.hash)
		byHash[f.hash] = f
	}

	log.Info(ctx, "cloudflare pages: requesting upload token")
	jwt, err := u.getUploadToken(ctx, project)
	if err != nil {
		return nil, errors.Wrap(err, "upload-token")
	}

	log.Info(ctx, "cloudflare pages: check-missing against %d hashes", len(allHashes))
	missing, err := u.checkMissing(ctx, jwt, allHashes)
	if err != nil {
		return nil, errors.Wrap(err, "check-missing")
	}
	log.Info(ctx, "cloudflare pages: %d missing assets to upload (reused %d)", len(missing), len(allHashes)-len(missing))

	if len(missing) > 0 {
		toUpload := make([]pagesFile, 0, len(missing))
		for _, h := range missing {
			if f, ok := byHash[h]; ok {
				toUpload = append(toUpload, f)
			}
		}
		if err := u.uploadAssets(ctx, jwt, toUpload); err != nil {
			return nil, errors.Wrap(err, "upload assets")
		}
	}

	log.Info(ctx, "cloudflare pages: upsert-hashes for the manifest side")
	if err := u.upsertHashes(ctx, jwt, allHashes); err != nil {
		return nil, errors.Wrap(err, "upsert-hashes")
	}

	manifest := make(map[string]string, len(files))
	for _, f := range files {
		manifest[f.relPath] = f.hash
	}
	log.Info(ctx, "cloudflare pages: creating deployment (project=%q, branch=%q)", project, branch)
	res, err := u.createDeployment(ctx, project, branch, commitMessage, commitHash, manifest)
	if err != nil {
		return nil, errors.Wrap(err, "create deployment")
	}
	log.Info(ctx, "cloudflare pages: deployed id=%s url=%s", res.ID, res.URL)
	return res, nil
}
