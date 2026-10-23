// Discover SA files and validate local credentials without issuing token requests.
package drive

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rclone/rclone/lib/env"
	"golang.org/x/oauth2/google"
)

// Keep lexical absolute paths as identities. Directory symlinks are not walked;
// file symlinks retain their own paths, matching explicit account-file selection.
func serviceAccountPath(name string) (string, error) {
	name = env.ShellExpand(name)
	if name == "" {
		return "", nil
	}
	return filepath.Abs(filepath.Clean(name))
}

func discoverServiceAccountFiles(root string) ([]string, error) {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("read service account directory %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("read service account directory %q: not a directory", root)
	}
	// Resolve only the root for traversal: return paths under the configured root
	// so a root symlink and an explicitly selected file share the same identity.
	walkRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	files := []string{}
	err = filepath.WalkDir(walkRoot, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("read service account directory %q: %w", name, walkErr)
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		rel, err := filepath.Rel(walkRoot, name)
		if err != nil {
			return err
		}
		files = append(files, filepath.Join(root, rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("service account directory %q contains no JSON files", root)
	}
	return files, nil
}

// Local credential failures are distinct from API quota/auth failures. Never
// include JSON/parser errors here: they can contain private credential material.
type serviceAccountFileError struct{ path, reason string }

func (e *serviceAccountFileError) Error() string {
	return fmt.Sprintf("service account credentials %q: %s", e.path, e.reason)
}

func validateServiceAccountJSON(data []byte) error {
	var key struct {
		Type       string `json:"type"`
		Email      string `json:"client_email"`
		PrivateKey string `json:"private_key"`
	}
	if json.Unmarshal(data, &key) != nil {
		return errors.New("invalid JSON")
	}
	if key.Type != "service_account" {
		return errors.New("expected type service_account")
	}
	if strings.TrimSpace(key.Email) == "" {
		return errors.New("missing client_email")
	}
	// Use the same schema as client construction, including optional fields.
	// Otherwise a malformed token_uri/private_key_id could pass discovery but
	// fail later with an unclassified parser error and block healthy candidates.
	conf, err := google.JWTConfigFromJSON(data)
	if err != nil {
		return errors.New("invalid service account field types")
	}
	endpoint, err := url.Parse(conf.TokenURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return errors.New("invalid token_uri")
	}
	raw := []byte(key.PrivateKey)
	if block, _ := pem.Decode(raw); block != nil {
		raw = block.Bytes
	}
	parsed, err := x509.ParsePKCS8PrivateKey(raw)
	if err != nil {
		parsed, err = x509.ParsePKCS1PrivateKey(raw)
	}
	if err != nil {
		return errors.New("invalid RSA private key")
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return errors.New("expected RSA private key")
	}
	if rsaKey.Validate() != nil {
		return errors.New("invalid RSA private key")
	}
	return nil
}

func inspectServiceAccountFile(name string) error {
	info, err := os.Stat(name)
	if err != nil {
		return &serviceAccountFileError{name, "file unavailable"}
	}
	if !info.Mode().IsRegular() {
		return &serviceAccountFileError{name, "not a regular file"}
	}
	return nil
}

func readServiceAccountFile(name string) ([]byte, error) {
	if err := inspectServiceAccountFile(name); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(name)
	if err != nil {
		return nil, &serviceAccountFileError{name, "file unreadable"}
	}
	if err = validateServiceAccountJSON(data); err != nil {
		return nil, &serviceAccountFileError{name, err.Error()}
	}
	return data, nil
}
