package publish

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Screenshot limits, checked before any network call.
const (
	MaxScreenshots     = 10
	MaxScreenshotBytes = 10 << 20
)

// ScreenshotExts are the attachable file types: those stdlib can decode and re-encode.
var ScreenshotExts = []string{".png", ".jpg", ".jpeg", ".gif"}

func checkScreenshots(paths []string) error {
	if len(paths) > MaxScreenshots {
		return fmt.Errorf("too many screenshots: %d (GitHub uploads are limited here to %d per report)", len(paths), MaxScreenshots)
	}
	for _, p := range paths {
		if !slices.Contains(ScreenshotExts, strings.ToLower(filepath.Ext(p))) {
			return fmt.Errorf("%s: not a supported screenshot type (%s)", filepath.Base(p), strings.Join(ScreenshotExts, ", "))
		}
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if fi.Size() > MaxScreenshotBytes {
			return fmt.Errorf("%s is %d MiB: each screenshot must be at most 10 MiB to upload", filepath.Base(p), fi.Size()>>20)
		}
	}
	return nil
}

// screenshotData prepares exactly the bytes and filename that will be uploaded.
func screenshotData(p string) ([]byte, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, "", err
	}
	img, _, err := image.Decode(f) // first frame only for GIF
	f.Close()
	if err != nil {
		return nil, "", fmt.Errorf("%s: not a readable image: %w", filepath.Base(p), err)
	}
	var b bytes.Buffer
	if ext := strings.ToLower(filepath.Ext(p)); ext == ".jpg" || ext == ".jpeg" {
		err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 92})
	} else {
		p = strings.TrimSuffix(p, filepath.Ext(p)) + ".png"
		err = png.Encode(&b, img)
	}
	return b.Bytes(), filepath.Base(p), err
}

func screenshotDestination(dir, name string, i int, previous []string) string {
	dst := filepath.Join(dir, name)
	for n := i; slices.Contains(previous, dst); n++ {
		dst = filepath.Join(dir, fmt.Sprintf("%d-%s", n, name))
	}
	return dst
}

// copyScreenshots re-encodes the files into dir (0600) so no EXIF/text metadata survives
// and a resume survives the originals moving. Undecodable files are rejected.
// Copies never overwrite a path in previous.
func copyScreenshots(dir string, paths, previous []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var out []string
	for i, p := range paths {
		data, name, err := screenshotData(p)
		if err != nil {
			return nil, err
		}
		dst := screenshotDestination(dir, name, i, append(slices.Clip(previous), out...))
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return nil, err
		}
		out = append(out, dst)
	}
	return out, nil
}

// sameScreenshots compares the ordered upload names and bytes, not original paths:
// saved drafts point at metadata-stripped copies, while Submit receives the originals.
func sameScreenshots(saved, reviewed []string) (bool, error) {
	if len(saved) != len(reviewed) {
		return false, nil
	}
	for i, p := range reviewed {
		want, err := os.ReadFile(saved[i])
		if err != nil {
			return false, err
		}
		if p == saved[i] {
			continue // already the draft copy; do not lossy-encode a JPEG twice
		}
		got, name, err := screenshotData(p)
		if err != nil {
			return false, err
		}
		dst := screenshotDestination(filepath.Dir(saved[i]), name, i, saved[:i])
		if dst != saved[i] || !bytes.Equal(want, got) {
			return false, nil
		}
	}
	return true, nil
}

// attachArgs returns gh --attach flags for every screenshot.
func attachArgs(paths []string) []string {
	var a []string
	for _, p := range paths {
		a = append(a, "--attach", p)
	}
	return a
}
