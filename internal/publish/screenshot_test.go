package publish

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyScreenshotsStripsMetadata(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})

	var pb bytes.Buffer
	png.Encode(&pb, img)
	raw := pb.Bytes()
	data := []byte("tEXtAuthor\x00secretuser")
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(data)-4))
	chunk = append(chunk, data...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(data))
	ihdrEnd := 8 + 25 // signature + IHDR chunk
	withText := append(append(append([]byte{}, raw[:ihdrEnd]...), chunk...), raw[ihdrEnd:]...)

	var jb bytes.Buffer
	jpeg.Encode(&jb, img, nil)
	exif := []byte("Exif\x00\x00GPS-secretmarker")
	app1 := append([]byte{0xFF, 0xE1}, binary.BigEndian.AppendUint16(nil, uint16(len(exif)+2))...)
	withExif := append(append(append([]byte{}, jb.Bytes()[:2]...), append(app1, exif...)...), jb.Bytes()[2:]...)

	pp, jp, bad := filepath.Join(src, "a.png"), filepath.Join(src, "b.jpg"), filepath.Join(src, "c.png")
	os.WriteFile(pp, withText, 0o600)
	os.WriteFile(jp, withExif, 0o600)
	os.WriteFile(bad, []byte("not an image"), 0o600)
	for p, secret := range map[string]string{pp: "secretuser", jp: "secretmarker"} {
		if b, _ := os.ReadFile(p); !bytes.Contains(b, []byte(secret)) {
			t.Fatalf("fixture %s lacks %q", p, secret)
		}
		if _, _, err := image.Decode(bytes.NewReader(withText)); err != nil {
			t.Fatalf("fixture does not decode: %v", err)
		}
	}

	out, err := copyScreenshots(dst, []string{pp, jp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, secret := range []string{"secretuser", "secretmarker"} {
		b, _ := os.ReadFile(out[i])
		if bytes.Contains(b, []byte(secret)) || bytes.Contains(b, []byte("Exif")) {
			t.Errorf("%s still carries metadata", out[i])
		}
		if _, _, err := image.Decode(bytes.NewReader(b)); err != nil {
			t.Errorf("%s does not decode: %v", out[i], err)
		}
	}
	if _, err := copyScreenshots(dst, []string{bad}, nil); err == nil {
		t.Error("undecodable file accepted")
	}
}
