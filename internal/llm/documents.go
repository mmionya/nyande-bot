package llm

import (
	"encoding/json"

	"github.com/mmionya/nyande-bot/internal/documents"
	"github.com/mmionya/nyande-bot/resources"
)

// JSON escaping keeps attachment text and filenames inside their data boundary.
func documentContext(source []documents.Document, imageBudget int) (string, []Image) {
	if len(source) == 0 {
		return "", nil
	}
	remaining := documents.MaxRequestRunes
	var images []Image
	var docs []documents.Document
	for _, doc := range source[:min(len(source), documents.MaxDocuments)] {
		text := []rune(doc.Text)
		if len(text) > remaining {
			text = text[:remaining]
			doc.Truncated = true
		}
		doc.Text = string(text)
		remaining -= len(text)
		name := []rune(doc.Name)
		doc.Name = string(name[:min(len(name), 255)])
		count := min(len(doc.Images), max(0, imageBudget-len(images)))
		if count < len(doc.Images) {
			doc.Warning += " " + resources.Get("documents.warning.image_budget")
			doc.ImageNames = doc.ImageNames[:min(len(doc.ImageNames), count)]
		}
		for _, image := range doc.Images[:count] {
			images = append(images, Image{MIME: image.MIME, Data: image.Data})
		}
		docs = append(docs, doc)
	}
	data, _ := json.Marshal(docs)
	return resources.Format("llm.prompt.documents", map[string]any{"documents": string(data)}), images
}
