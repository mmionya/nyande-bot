package documents

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/mmionya/nyande-bot/resources"
)

func attr(element xml.StartElement, name string) string {
	for _, a := range element.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func (a *documentArchive) tokens(name string, visit func(xml.Token) error) error {
	data, err := a.read(name)
	if err != nil {
		return err
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	for {
		if err := a.ctx.Err(); err != nil {
			return err
		}
		t, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New(resources.Get("documents.error.invalid_office"))
		}
		if err := visit(t); err != nil {
			return err
		}
	}
}

// Read text runs in order, retaining paragraph, table and slide boundaries.
func (a *documentArchive) richText(name string, out *textBuffer, odf bool) error {
	textDepth := 0
	tableDepth := 0
	return a.tokens(name, func(token xml.Token) error {
		switch t := token.(type) {
		case xml.StartElement:
			if (!odf && t.Name.Local == "t") || (odf && (t.Name.Local == "p" || t.Name.Local == "h" || t.Name.Local == "annotation")) {
				textDepth++
			}
			switch t.Name.Local {
			case "table":
				if odf {
					tableDepth++
					return out.add("\n[Sheet: " + attr(t, "name") + "]\n")
				}
			case "page":
				if odf {
					return out.add("\n[Slide: " + attr(t, "name") + "]\n")
				}
			case "table-cell":
				if odf && tableDepth > 0 {
					if repeated := attr(t, "number-columns-repeated"); repeated != "" && repeated != "1" {
						if err := out.add("[" + repeated + " repeated cells] "); err != nil {
							return err
						}
					}
					// Numeric, date and boolean values may have no displayed paragraph.
					for _, key := range []string{"value", "date-value", "time-value", "boolean-value", "string-value"} {
						if v := attr(t, key); v != "" {
							return out.add("[value: " + v + "] ")
						}
					}
				}
			case "table-row":
				if odf {
					if repeated := attr(t, "number-rows-repeated"); repeated != "" && repeated != "1" {
						return out.add("[" + repeated + " repeated rows]\n")
					}
				}
			case "tab":
				return out.add("\t")
			case "br", "cr", "line-break":
				return out.add("\n")
			case "s":
				if odf && textDepth > 0 {
					n, _ := strconv.Atoi(attr(t, "c"))
					return out.add(strings.Repeat(" ", max(1, min(n, 100))))
				}
			}
		case xml.EndElement:
			if (!odf && t.Name.Local == "t") || (odf && (t.Name.Local == "p" || t.Name.Local == "h" || t.Name.Local == "annotation")) {
				textDepth--
			}
			switch t.Name.Local {
			case "table":
				if odf {
					tableDepth--
				}
			case "p", "h", "tr", "table-row", "annotation":
				return out.add("\n")
			case "tc", "table-cell", "covered-table-cell":
				return out.add("\t")
			}
		case xml.CharData:
			if textDepth > 0 {
				return out.add(string(t))
			}
		}
		return nil
	})
}

// Resolve only package-local relationships; never fetch external content.
func (a *documentArchive) relationships(part string) (map[string]string, error) {
	rels := make(map[string]string)
	err := a.tokens(path.Join(path.Dir(part), "_rels", path.Base(part)+".rels"), func(token xml.Token) error {
		t, ok := token.(xml.StartElement)
		if !ok || t.Name.Local != "Relationship" || strings.EqualFold(attr(t, "TargetMode"), "External") {
			return nil
		}
		target := attr(t, "Target")
		if strings.HasPrefix(target, "/") {
			target = path.Clean(strings.TrimPrefix(target, "/"))
		} else {
			target = path.Join(path.Dir(part), target)
		}
		if target == ".." || strings.HasPrefix(target, "../") || strings.Contains(target, ":") {
			return nil
		}
		rels[attr(t, "Id")] = target
		return nil
	})
	return rels, err
}

func (a *documentArchive) presentation(out *textBuffer) error {
	const part = "ppt/presentation.xml"
	rels, err := a.relationships(part)
	if err != nil {
		return err
	}
	var slides []string
	err = a.tokens(part, func(token xml.Token) error {
		if t, ok := token.(xml.StartElement); ok && t.Name.Local == "sldId" {
			// The numeric id and the relationship id share a local name.
			for _, id := range t.Attr {
				if id.Name.Local == "id" && id.Name.Space != "" {
					slides = append(slides, rels[id.Value])
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i, name := range slides {
		if err := out.add(fmt.Sprintf("\n[Slide %d]\n", i+1)); err != nil {
			return err
		}
		if err := a.richText(name, out, false); err != nil {
			return err
		}
	}
	return nil
}

func (a *documentArchive) sharedStrings(name string) ([]string, error) {
	var values []string
	var value strings.Builder
	inText := false
	err := a.tokens(name, func(token xml.Token) error {
		switch t := token.(type) {
		case xml.StartElement:
			if t.Name.Local == "si" {
				value.Reset()
			}
			if t.Name.Local == "t" {
				inText = true
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				inText = false
			}
			if t.Name.Local == "si" {
				values = append(values, value.String())
			}
		case xml.CharData:
			if inText {
				value.Write(t)
			}
		}
		return nil
	})
	return values, err
}

func (a *documentArchive) spreadsheet(out *textBuffer) error {
	const part = "xl/workbook.xml"
	rels, err := a.relationships(part)
	if err != nil {
		return err
	}
	var shared []string
	if a.files["xl/sharedStrings.xml"] != nil {
		shared, err = a.sharedStrings("xl/sharedStrings.xml")
		if err != nil {
			return err
		}
	}
	type sheet struct{ name, target string }
	var sheets []sheet
	err = a.tokens(part, func(token xml.Token) error {
		if t, ok := token.(xml.StartElement); ok && t.Name.Local == "sheet" {
			sheets = append(sheets, sheet{attr(t, "name"), rels[attr(t, "id")]})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, sheet := range sheets {
		if err := out.add("\n[Sheet: " + sheet.name + "]\n"); err != nil {
			return err
		}
		if err := a.worksheet(sheet.target, shared, out); err != nil {
			return err
		}
	}
	return nil
}

func (a *documentArchive) worksheet(name string, shared []string, out *textBuffer) error {
	var kind, ref, field string
	var value, formula strings.Builder
	return a.tokens(name, func(token xml.Token) error {
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "c":
				kind, ref = attr(t, "t"), attr(t, "r")
				value.Reset()
				formula.Reset()
			case "v", "t", "f":
				field = t.Name.Local
			}
		case xml.CharData:
			switch field {
			case "v", "t":
				value.Write(t)
			case "f":
				formula.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t", "f":
				field = ""
			case "row":
				return out.add("\n")
			case "c":
				text := value.String()
				if kind == "s" {
					i, err := strconv.Atoi(text)
					if err != nil || i < 0 || i >= len(shared) {
						return errors.New(resources.Get("documents.error.invalid_office"))
					}
					text = shared[i]
				} else if kind == "b" {
					if text == "1" {
						text = "TRUE"
					} else if text == "0" {
						text = "FALSE"
					}
				}
				if formula.Len() > 0 {
					text += " [formula: =" + formula.String() + "]"
				}
				if text != "" {
					return out.add(ref + ": " + text + "\t")
				}
			}
		}
		return nil
	})
}
