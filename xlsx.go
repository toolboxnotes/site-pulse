package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Minimal .xlsx writer: sheets of rows; numbers stay numbers, everything else is text.

type xlsxSheet struct {
	Name string  `json:"name"`
	Rows [][]any `json:"rows"`
}

func xmlEsc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

var badSheetChars = regexp.MustCompile(`[\[\]\*\?/\\:]`)

func buildXLSX(sheets []xlsxSheet) ([]byte, error) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	add := func(name, body string) error {
		w, err := z.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, body)
		return err
	}
	var ct, wb, rels strings.Builder
	ct.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	wb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	rels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	used := map[string]bool{}
	for i, sh := range sheets {
		n := i + 1
		name := strings.TrimSpace(badSheetChars.ReplaceAllString(sh.Name, " "))
		if r := []rune(name); len(r) > 31 {
			name = string(r[:31])
		}
		if name == "" || used[name] {
			name = fmt.Sprintf("Sheet%d", n)
		}
		used[name] = true
		ct.WriteString(fmt.Sprintf(`<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n))
		wb.WriteString(fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEsc(name), n, n))
		rels.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n))
		// column widths from content
		widths := map[int]int{}
		for _, row := range sh.Rows {
			for c, v := range row {
				l := len([]rune(fmt.Sprint(v)))
				if l > widths[c] {
					widths[c] = l
				}
			}
		}
		var s strings.Builder
		s.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>`)
		if len(widths) > 0 {
			s.WriteString("<cols>")
			for c := 0; c < len(widths); c++ {
				w := min(max(widths[c]+2, 8), 60)
				s.WriteString(fmt.Sprintf(`<col min="%d" max="%d" width="%d" customWidth="1"/>`, c+1, c+1, w))
			}
			s.WriteString("</cols>")
		}
		s.WriteString("<sheetData>")
		for r, row := range sh.Rows {
			s.WriteString(fmt.Sprintf(`<row r="%d">`, r+1))
			for c, v := range row {
				ref := fmt.Sprintf("%s%d", colName(c), r+1)
				style := ""
				if r == 0 {
					style = ` s="1"`
				}
				switch x := v.(type) {
				case nil:
				case float64:
					s.WriteString(fmt.Sprintf(`<c r="%s"%s><v>%v</v></c>`, ref, style, x))
				case bool:
					s.WriteString(fmt.Sprintf(`<c r="%s"%s t="inlineStr"><is><t>%v</t></is></c>`, ref, style, x))
				default:
					s.WriteString(fmt.Sprintf(`<c r="%s"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, xmlEsc(fmt.Sprint(x))))
				}
			}
			s.WriteString("</row>")
		}
		s.WriteString("</sheetData></worksheet>")
		if err := add(fmt.Sprintf("xl/worksheets/sheet%d.xml", n), s.String()); err != nil {
			return nil, err
		}
	}
	ct.WriteString(`</Types>`)
	wb.WriteString(`</sheets></workbook>`)
	rels.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`, len(sheets)+1))
	styles := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts><fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills><borders count="1"><border/></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`
	for _, f := range [][2]string{
		{"[Content_Types].xml", ct.String()},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", wb.String()},
		{"xl/_rels/workbook.xml.rels", rels.String()},
		{"xl/styles.xml", styles},
	} {
		if err := add(f[0], f[1]); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var safeFile = regexp.MustCompile(`[^\p{L}\p{N}._ -]+`)

func (a *App) handleXLSX(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Filename string      `json:"filename"`
		Sheets   []xlsxSheet `json:"sheets"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 20<<20)).Decode(&in); err != nil || len(in.Sheets) == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	b, err := buildXLSX(in.Sheets)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	name := safeFile.ReplaceAllString(in.Filename, "_")
	if name == "" {
		name = "report"
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.xlsx"`)
	w.Write(b)
}
