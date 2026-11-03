package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestImageOnlyDocumentsAndImageLimits(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewNRGBA(image.Rect(0, 0, 2000, 1000))); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"word/document.xml": "<document><body/></document>"}
	for i := range 8 {
		files[fmt.Sprintf("word/media/image%d.png", i)] = picture.String()
	}
	files["word/media/bad.svg"] = "<svg/>"
	files["word/media/broken.png"] = "broken"
	data := archiveFixture(t, files)
	doc, err := Read(context.Background(), "pictures.docx", "", data, 6)
	if err != nil || doc.Text != "" || len(doc.Images) != 6 || !strings.Contains(doc.Warning, "6 of 10") {
		t.Fatalf("images=%d warning=%q err=%v", len(doc.Images), doc.Warning, err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(doc.Images[0].Data))
	if err != nil || cfg.Width != 1600 || cfg.Height != 800 {
		t.Fatalf("image resizing: %#v %v", cfg, err)
	}
	_, err = Read(context.Background(), "pictures.docx", "", data, 0)
	if !errors.Is(err, ErrNoText) {
		t.Fatal(err)
	}
}

func archiveFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, text := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestOfficeTextAndEmbeddedPictures(t *testing.T) {
	for _, tc := range []struct {
		ext  string
		want []string
	}{
		{"docx", []string{"Привет, кот!", "Revenue", "42"}},
		{"pptx", []string{"[Slide 1]", "[Slide 2]", "Привет, кот!"}},
		{"xlsx", []string{"[Sheet: Budget]", "A1: Revenue", "B1: 42", "C1: TRUE", "=B1*2", "[Sheet: Notes]", "Привет, кот!"}},
		{"odt", []string{"Привет, кот!", "Document sample"}},
		{"ods", []string{"[Sheet: Budget]", "Revenue", "42"}},
		{"odp", []string{"[Slide: Slide 1]", "Привет, кот!"}},
		{"odf", []string{"a + b"}},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			for _, vision := range []int{0, 6} {
				doc, err := Read(context.Background(), "sample."+tc.ext, "application/octet-stream", fixture(t, "sample."+tc.ext), vision)
				if err != nil {
					t.Fatal(err)
				}
				for _, text := range tc.want {
					if !strings.Contains(doc.Text, text) {
						t.Errorf("missing %q in %q", text, doc.Text)
					}
				}
				if doc.Truncated {
					t.Fatal("small document truncated")
				}
				if vision == 0 {
					if len(doc.Images) != 0 {
						t.Fatal("vision disabled")
					}
					continue
				}
				if len(doc.Images) != 1 || len(doc.ImageNames) != 1 {
					t.Fatalf("missing embedded image: %#v", doc)
				}
				im, _, err := image.Decode(bytes.NewReader(doc.Images[0].Data))
				if err != nil {
					t.Fatal(err)
				}
				r, g, b, _ := im.At(0, 0).RGBA()
				if r <= g || r <= b {
					t.Fatal("embedded red picture changed")
				}
			}
		})
	}
}

func TestPDFTextAndScannedImages(t *testing.T) {
	for _, binary := range []string{"pdftotext", "pdfinfo", "pdftoppm"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip("poppler-utils not installed")
		}
	}
	doc, err := Read(context.Background(), "sample.pdf", "", fixture(t, "sample.pdf"), 0)
	if err != nil || !strings.Contains(doc.Text, "Document sample page 7") || len(doc.Images) != 0 {
		t.Fatalf("PDF text: %#v %v", doc, err)
	}
	doc, err = Read(context.Background(), "sample.pdf", "", fixture(t, "sample.pdf"), 2)
	if err != nil || len(doc.Images) != 2 || !strings.Contains(doc.Warning, "of 7") {
		t.Fatalf("PDF image limit: %d %s %v", len(doc.Images), doc.Warning, err)
	}
	doc, err = Read(context.Background(), "scanned.pdf", "", fixture(t, "scanned.pdf"), 6)
	if err != nil || len(doc.Images) != 1 || doc.Text != "" {
		t.Fatalf("scan: images=%d text=%q err=%v", len(doc.Images), doc.Text, err)
	}
	im, _, err := image.Decode(bytes.NewReader(doc.Images[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := im.At(im.Bounds().Dx()/4, im.Bounds().Dy()*3/4).RGBA()
	if r <= g || r <= b {
		t.Fatal("PDF embedded red picture was not rendered")
	}
	_, err = Read(context.Background(), "scanned.pdf", "", fixture(t, "scanned.pdf"), 0)
	if !errors.Is(err, ErrNoText) {
		t.Fatalf("scan without vision: %v", err)
	}
}

func TestDocumentLimitsAndErrors(t *testing.T) {
	for _, ext := range []string{"pdf", "docx", "pptx", "xlsx", "odt", "ods", "odp", "odf"} {
		if !Supported("REPORT."+strings.ToUpper(ext), "") {
			t.Errorf("extension %s", ext)
		}
		if _, err := Extract(context.Background(), "bad."+ext, "", []byte("broken")); err == nil {
			t.Errorf("accepted broken %s", ext)
		}
	}
	if !Supported("download.bin", "application/pdf; charset=binary") || Supported("virus.exe", "application/octet-stream") {
		t.Fatal("MIME detection")
	}
	data := archiveFixture(t, map[string]string{"word/document.xml": "<document><p><t>" + strings.Repeat("猫", MaxTextRunes+1) + "</t></p></document>"})
	doc, err := Extract(context.Background(), "large.docx", "", data)
	if err != nil || !doc.Truncated || utf8.RuneCountInString(doc.Text) != MaxTextRunes || !utf8.ValidString(doc.Text) {
		t.Fatalf("truncation: %d %v %v", len(doc.Text), doc.Truncated, err)
	}
	_, err = Extract(context.Background(), "large.pdf", "", make([]byte, MaxFileSize+1))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Read(ctx, "sample.docx", "", fixture(t, "sample.docx"), 6)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = Extract(context.Background(), "bad.docx", "", archiveFixture(t, map[string]string{"word/document.xml": "<document><p>"}))
	if err == nil {
		t.Fatal("malformed XML accepted")
	}
	// Reject a compact ZIP bomb before decompressing its XML.
	data = archiveFixture(t, map[string]string{"word/document.xml": strings.Repeat(" ", maxExpandedSize+1)})
	_, err = Extract(context.Background(), "bomb.docx", "", data)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

func TestSharedStringsAndSlideOrder(t *testing.T) {
	xlsx := archiveFixture(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="urn:rels"><sheets><sheet name="Data" r:id="r1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="r1" Target="/xl/worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><r><t>Rich </t></r><r><t>text</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row><c r="C4" t="s"><v>0</v></c></row></sheetData></worksheet>`,
	})
	doc, err := Extract(context.Background(), "test.xlsx", "", xlsx)
	if err != nil || !strings.Contains(doc.Text, "C4: Rich text") {
		t.Fatalf("shared strings: %q %v", doc.Text, err)
	}
	pptx := archiveFixture(t, map[string]string{
		"ppt/presentation.xml":            `<presentation xmlns:r="urn:rels"><sldIdLst><sldId id="2" r:id="second"/><sldId id="1" r:id="first"/></sldIdLst></presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships><Relationship Id="first" Target="slides/slide1.xml"/><Relationship Id="second" Target="slides/slide2.xml"/></Relationships>`,
		"ppt/slides/slide1.xml":           `<slide><p><t>First file</t></p></slide>`,
		"ppt/slides/slide2.xml":           `<slide><p><t>Second file</t></p></slide>`,
	})
	doc, err = Extract(context.Background(), "test.pptx", "", pptx)
	if err != nil || strings.Index(doc.Text, "Second file") > strings.Index(doc.Text, "First file") {
		t.Fatalf("slide order: %q %v", doc.Text, err)
	}
}
