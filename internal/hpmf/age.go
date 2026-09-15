package hpmf

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

// PackAndEncrypt tars+gzips the directory tree rooted at dir, encrypts
// the result to recipients with age, and writes it to outPath — the
// "temp location -> single .age file" step MEMORY_FORMAT.md's storage
// model describes. dir is never the long-lived artifact; outPath is.
func PackAndEncrypt(dir, outPath string, recipients []age.Recipient) error {
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("hpmf: create %s: %w", outPath, err)
	}
	defer out.Close()

	ageWriter, err := age.Encrypt(out, recipients...)
	if err != nil {
		return fmt.Errorf("hpmf: start age encryption: %w", err)
	}
	gzWriter := gzip.NewWriter(ageWriter)
	tarWriter := tar.NewWriter(gzWriter)

	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tarWriter.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tarWriter, f)
		return err
	})
	if err != nil {
		return fmt.Errorf("hpmf: pack %s: %w", dir, err)
	}

	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("hpmf: finish tar: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return fmt.Errorf("hpmf: finish gzip: %w", err)
	}
	if err := ageWriter.Close(); err != nil {
		return fmt.Errorf("hpmf: finish age encryption: %w", err)
	}
	return nil
}

// DecryptAndUnpack reverses PackAndEncrypt: decrypts inPath with
// identities, then untars+ungzips into destDir (created if needed).
func DecryptAndUnpack(inPath, destDir string, identities []age.Identity) error {
	in, err := os.Open(inPath)
	if err != nil {
		return fmt.Errorf("hpmf: open %s: %w", inPath, err)
	}
	defer in.Close()

	ageReader, err := age.Decrypt(in, identities...)
	if err != nil {
		return fmt.Errorf("hpmf: decrypt %s (wrong identity/passphrase?): %w", inPath, err)
	}
	gzReader, err := gzip.NewReader(ageReader)
	if err != nil {
		return fmt.Errorf("hpmf: read gzip stream: %w", err)
	}
	defer gzReader.Close()
	tarReader := tar.NewReader(gzReader)

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("hpmf: create %s: %w", destDir, err)
	}

	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("hpmf: read tar entry: %w", err)
		}
		// Reject path traversal in a maliciously (or corruptly) crafted
		// archive — every entry must resolve to strictly inside destDir.
		target := filepath.Join(destDir, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("hpmf: tar entry %q escapes destination directory", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("hpmf: create directory %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("hpmf: create directory for %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return fmt.Errorf("hpmf: create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tarReader); err != nil {
				f.Close()
				return fmt.Errorf("hpmf: write %s: %w", target, err)
			}
			f.Close()
		}
	}
	return nil
}
