package provider

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image/color"
	"os"
	"strings"
	"testing"
)

// buildSolidColorPNG constructs a small, valid PNG of one solid color —
// a real, minimal test image (not a byte-identical-to-production
// fixture) for live-verifying DescribeImage actually produces a sensible
// caption against the real vendor APIs, not just a canned httptest
// response (see vision_test.go for the wire-shape-only tests).
func buildSolidColorPNG(t *testing.T, size int, c color.RGBA) []byte {
	t.Helper()
	chunk := func(tag string, data []byte) []byte {
		var buf bytes.Buffer
		binary.Write(&buf, binary.BigEndian, uint32(len(data)))
		buf.WriteString(tag)
		buf.Write(data)
		crc := crc32.ChecksumIEEE(append([]byte(tag), data...))
		binary.Write(&buf, binary.BigEndian, crc)
		return buf.Bytes()
	}

	var ihdrData bytes.Buffer
	binary.Write(&ihdrData, binary.BigEndian, uint32(size))
	binary.Write(&ihdrData, binary.BigEndian, uint32(size))
	ihdrData.Write([]byte{8, 2, 0, 0, 0}) // 8-bit depth, color type 2 (RGB), default filter/interlace

	var raw bytes.Buffer
	for y := 0; y < size; y++ {
		raw.WriteByte(0) // no filter
		for x := 0; x < size; x++ {
			raw.Write([]byte{c.R, c.G, c.B})
		}
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	var png bytes.Buffer
	png.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	png.Write(chunk("IHDR", ihdrData.Bytes()))
	png.Write(chunk("IDAT", compressed.Bytes()))
	png.Write(chunk("IEND", nil))
	return png.Bytes()
}

// TestLiveDescribeImageAgainstRealOpenAI/TestLiveDescribeImageAgainstRealAnthropic
// are live-verification tests for the file-ingestion feature's image
// captioning (internal/gateway/attachments.go's describeImageAttachment):
// confirms DescribeImage's wire format (vision_test.go's
// httptest-asserted shape) actually produces a sensible caption against
// the real vendor APIs, not just a mocked response — the real risk this
// guards against is a wire-format assumption that happens to satisfy a
// canned test server but not the real API. Skipped unless the
// corresponding API key is set.
func TestLiveDescribeImageAgainstRealOpenAI(t *testing.T) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	data := buildSolidColorPNG(t, 64, color.RGBA{R: 220, G: 20, B: 20, A: 255})
	p := NewOpenAICompat(OpenAICompatConfig{
		Name: "live", Vendor: "openai", Model: "gpt-4.1", BaseURL: "https://api.openai.com/v1", APIKey: apiKey,
	})
	caption, err := p.DescribeImage(context.Background(), ImageInput{Data: data, MIMEType: "image/png"}, imageDescribeInstructionForLiveTest)
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	t.Logf("caption: %s", caption)
	if !strings.Contains(strings.ToLower(caption), "red") {
		t.Errorf("caption = %q, want it to mention the color red", caption)
	}
}

func TestLiveDescribeImageAgainstRealAnthropic(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}
	data := buildSolidColorPNG(t, 64, color.RGBA{R: 220, G: 20, B: 20, A: 255})
	p := NewAnthropic(AnthropicConfig{
		Name: "live", Vendor: "anthropic", Model: "claude-sonnet-5", BaseURL: "https://api.anthropic.com/v1", APIKey: apiKey, APIVersion: "2023-06-01",
	})
	caption, err := p.DescribeImage(context.Background(), ImageInput{Data: data, MIMEType: "image/png"}, imageDescribeInstructionForLiveTest)
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	t.Logf("caption: %s", caption)
	if !strings.Contains(strings.ToLower(caption), "red") {
		t.Errorf("caption = %q, want it to mention the color red", caption)
	}
}

const imageDescribeInstructionForLiveTest = "Describe this image factually and specifically: what it shows, any visible text, notable objects, people, or scenes. Be concise but concrete enough that someone who can't see the image would understand what's in it."
