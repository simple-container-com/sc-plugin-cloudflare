// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudflare

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"

	. "github.com/onsi/gomega"
)

// TestHashFile_WranglerCompatible pins the file-hash algorithm against the
// shape wrangler uses. If this test fails, every pre-uploaded asset's key
// in Cloudflare's cache collides and Pages rejects the upsert step with a
// cryptic 4xx. The algorithm is: BLAKE3(base64(content) + ext-no-dot) → hex,
// first 32 characters.
//
// Reference: cloudflare/workers-sdk
// packages/deploy-helpers/src/deploy/helpers/hash.ts.
func TestHashFile_WranglerCompatible(t *testing.T) {
	RegisterTestingT(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "index.html")
	content := []byte("<h1>hi</h1>")
	Expect(os.WriteFile(path, content, 0o644)).To(Succeed())

	got, err := hashFile(path)
	Expect(err).ToNot(HaveOccurred())

	// Hand-compute the expected hash the same way wrangler does.
	b64 := base64.StdEncoding.EncodeToString(content)
	sum := blake3.Sum256([]byte(b64 + "html"))
	expected := hex.EncodeToString(sum[:])[:32]
	Expect(got).To(Equal(expected))
	Expect(got).To(HaveLen(32), "wrangler truncates to 32 hex characters — don't skip the slice")
}

// TestHashFile_ExtensionAffectsHash — two files with the same content but
// different extensions must hash differently. The dot is stripped before
// concatenation (`index.html` → `html`, not `.html`). Catches a shape
// drift that would otherwise silently invalidate every stored asset.
func TestHashFile_ExtensionAffectsHash(t *testing.T) {
	RegisterTestingT(t)

	dir := t.TempDir()
	htmlPath := filepath.Join(dir, "a.html")
	txtPath := filepath.Join(dir, "a.txt")
	content := []byte("same bytes")
	Expect(os.WriteFile(htmlPath, content, 0o644)).To(Succeed())
	Expect(os.WriteFile(txtPath, content, 0o644)).To(Succeed())

	h1, err := hashFile(htmlPath)
	Expect(err).ToNot(HaveOccurred())
	h2, err := hashFile(txtPath)
	Expect(err).ToNot(HaveOccurred())
	Expect(h1).ToNot(Equal(h2),
		"same bytes under different extensions must hash differently — otherwise CF Pages conflates them")
}

// TestContentTypeOf — Go's mime stdlib must know common web MIME types.
// If this starts failing on a fresh runner, add a static override table.
func TestContentTypeOf(t *testing.T) {
	RegisterTestingT(t)

	Expect(contentTypeOf("/foo/index.html")).To(ContainSubstring("text/html"))
	Expect(contentTypeOf("/foo/style.css")).To(ContainSubstring("text/css"))
	Expect(contentTypeOf("/foo/app.js")).To(ContainSubstring("javascript"))
	Expect(contentTypeOf("/foo/image.svg")).To(ContainSubstring("svg"))
	Expect(contentTypeOf("/foo/no-extension")).To(Equal("application/octet-stream"))
}

// TestScanBundle — walking a bundle dir surfaces every regular file with a
// forward-slash-prefixed relative path, independent of the host OS path
// separator.
func TestScanBundle(t *testing.T) {
	RegisterTestingT(t)

	dir := t.TempDir()
	Expect(os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>hi</h1>"), 0o644)).To(Succeed())
	Expect(os.MkdirAll(filepath.Join(dir, "sub"), 0o755)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(dir, "sub", "page.html"), []byte("<p>sub</p>"), 0o644)).To(Succeed())

	files, err := scanBundle(dir)
	Expect(err).ToNot(HaveOccurred())
	Expect(files).To(HaveLen(2))

	paths := []string{files[0].relPath, files[1].relPath}
	Expect(paths).To(ContainElements("/index.html", "/sub/page.html"),
		"all rel paths are leading-slash + forward-slashes; manifest keys mirror the HTTP path operators expect to GET")
}
