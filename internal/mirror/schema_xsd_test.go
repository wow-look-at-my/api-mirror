package mirror

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reference XSD is not enforced at load time and CI only checks that it
// parses, so nothing stopped it describing a grammar the shipped specs had
// already outgrown. It had: <purge>, <key credential=>, <reveal><credential>
// and every operations element were missing, and the sample went on validating
// against nobody. This walks the specs and demands the XSD know each name.
//
// direction only. A name the XSD declares and no spec uses is not drift;
// a name a spec uses and the XSD does not know is the editor lying to whoever
// writes the next.

func TestReferenceSchemaKnowsEveryNameTheShippedSpecsUse(t *testing.T) {
	elements, attributes := declaredNames(t)

	for _, path := range shippedSpecs {
		t.Run(filepath.Base(path), func(t *testing.T) {
			usedElements, usedAttrs := namesUsedBy(t, path)
			for _, name := range usedElements {
				assert.Contains(t, elements, name,
					"<%s> is in the spec and not in mirror.schema.xsd", name)
			}
			for _, name := range usedAttrs {
				assert.Contains(t, attributes, name,
					"the attribute %q is in the spec and not in mirror.schema.xsd", name)
			}
		})
	}
}

// declaredNames reads every element and attribute name the XSD declares.
func declaredNames(t *testing.T) (elements, attributes []string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot, "mirror.schema.xsd"))
	require.NoError(t, err)

	dec := xml.NewDecoder(strings.NewReader(stripDecl(string(body))))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		name := attrOf(start, "name")
		if name == "" {
			continue
		}
		switch start.Name.Local {
		case "element":
			elements = append(elements, name)
		case "attribute":
			attributes = append(attributes, name)
		}
	}
	require.NotEmpty(t, elements, "the XSD parsed to nothing, so this test proves nothing")
	// A placeholder is api-dsl's, shared with api-cli, and appears in any
	// element's content rather than being declared per site.
	return append(elements, "value", "if", "for", "else"),
		append(attributes, "test", "eq", "each", "expr", "as", "default")
}

// namesUsedBy reads every element and attribute name spec actually uses.
func namesUsedBy(t *testing.T, path string) (elements, attributes []string) {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)

	dec := xml.NewDecoder(strings.NewReader(stripDecl(string(body))))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		elements = appendOnce(elements, start.Name.Local)
		for _, a := range start.Attr {
			if a.Name.Local == "xmlns" || a.Name.Space == "xmlns" {
				continue
			}
			attributes = appendOnce(attributes, a.Name.Local)
		}
	}
	require.NotEmpty(t, elements, "%s parsed to nothing", path)
	// The root and its schema pointer are the document, not the grammar.
	return removeString(elements, "mirror"), removeString(attributes, "schema")
}

// stripDecl drops an XML 1.1 declaration, which the stdlib decoder refuses.
func stripDecl(s string) string {
	if !strings.HasPrefix(s, "<?xml") {
		return s
	}
	if i := strings.Index(s, "?>"); i >= 0 {
		return s[i+2:]
	}
	return s
}

func attrOf(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func appendOnce(list []string, s string) []string {
	if containsString(list, s) {
		return list
	}
	return append(list, s)
}

func removeString(list []string, drop string) []string {
	out := list[:0]
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
