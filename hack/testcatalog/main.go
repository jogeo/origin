package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Metadata holds parsed test metadata extracted from bracketed tags in test names.
type Metadata struct {
	Sig          string
	Features     []string
	FeatureGates []string
	APIGroups    []string
	Suites       []string
	Tags         []string
	Timeout      string
	Requires     []string
	Skipped      []string
	Jira         string
	TestID       string
	Labels       []string
}

// TestCase represents a single extracted test.
type TestCase struct {
	Name     string
	FullName string
	File     string
	Line     int
	Source   string
	Metadata Metadata
}

// TestCatalog groups tests by first-level subdirectory.
type TestCatalog struct {
	Directory string
	Tests     []TestCase
}

// Ginkgo container functions (Describe, Context, When and focused/pending variants).
var containerFuncs = map[string]bool{
	"Describe": true, "FDescribe": true, "XDescribe": true,
	"Context": true, "FContext": true, "XContext": true,
	"When": true, "FWhen": true, "XWhen": true,
}

// Ginkgo leaf test functions (It, Specify and focused/pending variants).
var testFuncs = map[string]bool{
	"It": true, "FIt": true, "XIt": true,
	"Specify": true, "FSpecify": true, "XSpecify": true,
}

// findGinkgoAlias returns the import alias used for ginkgo/v2 in the file.
// Returns "" if ginkgo is not imported.
func findGinkgoAlias(file *ast.File) string {
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if strings.Contains(path, "onsi/ginkgo") {
			if imp.Name != nil {
				return imp.Name.Name
			}
			parts := strings.Split(path, "/")
			return parts[len(parts)-1]
		}
	}
	return ""
}

// isGinkgoCall checks if call is a ginkgo Describe/Context/It/etc. with the given alias.
// Returns the function name (e.g. "Describe", "It") or "".
func isGinkgoCall(call *ast.CallExpr, alias string) string {
	if alias == "." {
		if ident, ok := call.Fun.(*ast.Ident); ok {
			if containerFuncs[ident.Name] || testFuncs[ident.Name] {
				return ident.Name
			}
		}
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != alias {
		return ""
	}
	name := sel.Sel.Name
	if containerFuncs[name] || testFuncs[name] {
		return name
	}
	return ""
}

// extractStringValue attempts to statically resolve a string from an AST expression.
// Handles string literals, concatenation, and fmt.Sprintf format strings.
func extractStringValue(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			s, err := strconv.Unquote(e.Value)
			if err != nil {
				return ""
			}
			return s
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			left := extractStringValue(e.X)
			right := extractStringValue(e.Y)
			return left + right
		}
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok {
				if ident.Name == "fmt" && sel.Sel.Name == "Sprintf" && len(e.Args) > 0 {
					return extractStringValue(e.Args[0])
				}
			}
		}
	}
	return ""
}

// extractLabels pulls string arguments from Label() decorators in the call args.
func extractLabels(args []ast.Expr, alias string) []string {
	var labels []string
	for _, arg := range args {
		call, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		isLabel := false
		if alias == "." {
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "Label" {
				isLabel = true
			}
		} else if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == alias && sel.Sel.Name == "Label" {
				isLabel = true
			}
		}
		if isLabel {
			for _, la := range call.Args {
				if s := extractStringValue(la); s != "" {
					labels = append(labels, s)
				}
			}
		}
	}
	return labels
}

// walkGinkgoNodes recursively walks the AST to find Ginkgo Describe/Context/It calls,
// building up the full test name from the nesting hierarchy.
func walkGinkgoNodes(node ast.Node, fset *token.FileSet, alias string, describeStack []string, tests *[]TestCase, relFile string) {
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		funcName := isGinkgoCall(call, alias)
		if funcName == "" {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}

		text := extractStringValue(call.Args[0])
		labels := extractLabels(call.Args, alias)

		if containerFuncs[funcName] {
			if text == "" {
				text = "<dynamic>"
			}
			newStack := make([]string, len(describeStack)+1)
			copy(newStack, describeStack)
			newStack[len(describeStack)] = text

			for _, arg := range call.Args {
				if fl, ok := arg.(*ast.FuncLit); ok {
					walkGinkgoNodes(fl.Body, fset, alias, newStack, tests, relFile)
					break
				}
			}
			return false
		}

		if testFuncs[funcName] {
			if text == "" {
				text = "<dynamic>"
			}
			fullParts := make([]string, len(describeStack)+1)
			copy(fullParts, describeStack)
			fullParts[len(describeStack)] = text
			fullName := strings.Join(fullParts, " ")
			pos := fset.Position(call.Pos())

			metadata := parseMetadata(fullName)
			metadata.Labels = append(metadata.Labels, labels...)

			if strings.HasPrefix(funcName, "F") {
				metadata.Tags = append(metadata.Tags, "Focused")
			} else if strings.HasPrefix(funcName, "X") {
				metadata.Tags = append(metadata.Tags, "Pending")
			}

			*tests = append(*tests, TestCase{
				Name:     cleanName(fullName),
				FullName: fullName,
				File:     relFile,
				Line:     pos.Line,
				Source:   "ginkgo",
				Metadata: metadata,
			})
			return false
		}

		return true
	})
}

// extractTestsFromFile parses a Go source file and returns all Ginkgo tests found.
func extractTestsFromFile(filePath, relPath string) []TestCase {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filePath, nil, 0)
	if err != nil {
		return nil
	}

	alias := findGinkgoAlias(file)
	if alias == "" {
		return nil
	}

	var tests []TestCase
	walkGinkgoNodes(file, fset, alias, nil, &tests, relPath)
	return tests
}

// --- Metadata Parsing ---

var bracketRe = regexp.MustCompile(`\[([^\]]+)\]`)
var testIDRe = regexp.MustCompile(`^OCP-\d+$`)

func parseMetadata(fullName string) Metadata {
	var m Metadata
	for _, match := range bracketRe.FindAllStringSubmatch(fullName, -1) {
		tag := match[1]
		switch {
		case strings.HasPrefix(tag, "sig-"):
			m.Sig = strings.TrimPrefix(tag, "sig-")
		case strings.HasPrefix(tag, "Feature:"):
			m.Features = append(m.Features, strings.TrimPrefix(tag, "Feature:"))
		case strings.HasPrefix(tag, "FeatureGate:"):
			m.FeatureGates = append(m.FeatureGates, strings.TrimPrefix(tag, "FeatureGate:"))
		case strings.HasPrefix(tag, "OCPFeatureGate:"):
			m.FeatureGates = append(m.FeatureGates, strings.TrimPrefix(tag, "OCPFeatureGate:"))
		case strings.HasPrefix(tag, "apigroup:"):
			m.APIGroups = append(m.APIGroups, strings.TrimPrefix(tag, "apigroup:"))
		case strings.HasPrefix(tag, "Suite:"):
			m.Suites = append(m.Suites, strings.TrimPrefix(tag, "Suite:"))
		case strings.HasPrefix(tag, "Timeout:"):
			m.Timeout = strings.TrimPrefix(tag, "Timeout:")
		case strings.HasPrefix(tag, "Requires:"):
			m.Requires = append(m.Requires, strings.TrimPrefix(tag, "Requires:"))
		case strings.HasPrefix(tag, "Skipped:"):
			m.Skipped = append(m.Skipped, strings.TrimPrefix(tag, "Skipped:"))
		case strings.HasPrefix(tag, "Jira:"):
			m.Jira = strings.Trim(strings.TrimPrefix(tag, "Jira:"), `"`)
		case testIDRe.MatchString(tag):
			m.TestID = tag
		default:
			m.Tags = append(m.Tags, tag)
		}
	}
	return m
}

func cleanName(name string) string {
	result := bracketRe.ReplaceAllString(name, "")
	return strings.TrimSpace(strings.Join(strings.Fields(result), " "))
}

// --- YAML Output ---

func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

func writeYAMLList(b *strings.Builder, indent, key string, values []string) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(b, "%s%s:\n", indent, key)
	for _, v := range values {
		fmt.Fprintf(b, "%s  - %s\n", indent, yamlQuote(v))
	}
}

func writeYAMLFile(catalog TestCatalog, path string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Auto-generated test catalog for %s\n", catalog.Directory)
	fmt.Fprintf(&b, "# Generated by hack/testcatalog. Do not edit.\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# To add non-Ginkgo tests, create a file named %s_manual.yaml\n", catalog.Directory)
	fmt.Fprintf(&b, "# in this directory using the same schema (source: manual).\n")
	fmt.Fprintf(&b, "directory: %s\n", yamlQuote(catalog.Directory))
	fmt.Fprintf(&b, "tests:\n")

	for _, t := range catalog.Tests {
		fmt.Fprintf(&b, "  - name: %s\n", yamlQuote(t.Name))
		fmt.Fprintf(&b, "    full_name: %s\n", yamlQuote(t.FullName))
		fmt.Fprintf(&b, "    file: %s\n", yamlQuote(t.File))
		fmt.Fprintf(&b, "    line: %d\n", t.Line)
		fmt.Fprintf(&b, "    source: %s\n", t.Source)
		fmt.Fprintf(&b, "    metadata:\n")
		if t.Metadata.Sig != "" {
			fmt.Fprintf(&b, "      sig: %s\n", yamlQuote(t.Metadata.Sig))
		}
		writeYAMLList(&b, "      ", "features", t.Metadata.Features)
		writeYAMLList(&b, "      ", "feature_gates", t.Metadata.FeatureGates)
		writeYAMLList(&b, "      ", "apigroups", t.Metadata.APIGroups)
		writeYAMLList(&b, "      ", "suites", t.Metadata.Suites)
		writeYAMLList(&b, "      ", "tags", t.Metadata.Tags)
		if t.Metadata.Timeout != "" {
			fmt.Fprintf(&b, "      timeout: %s\n", yamlQuote(t.Metadata.Timeout))
		}
		writeYAMLList(&b, "      ", "requires", t.Metadata.Requires)
		writeYAMLList(&b, "      ", "skipped", t.Metadata.Skipped)
		if t.Metadata.Jira != "" {
			fmt.Fprintf(&b, "      jira: %s\n", yamlQuote(t.Metadata.Jira))
		}
		if t.Metadata.TestID != "" {
			fmt.Fprintf(&b, "      test_id: %s\n", yamlQuote(t.Metadata.TestID))
		}
		writeYAMLList(&b, "      ", "labels", t.Metadata.Labels)
	}

	return os.WriteFile(path, []byte(b.String()), 0644)
}

// --- Markdown Output ---

func mdCodeList(items []string) string {
	wrapped := make([]string, len(items))
	for i, item := range items {
		wrapped[i] = "`" + item + "`"
	}
	return strings.Join(wrapped, ", ")
}

func writeMarkdownFile(catalog TestCatalog, path string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Test Catalog: %s\n\n", catalog.Directory)
	fmt.Fprintf(&b, "> Auto-generated from Ginkgo test sources by `hack/testcatalog`.\n")
	fmt.Fprintf(&b, "> **Do not edit.** Non-Ginkgo tests can be added via YAML files with the same schema.\n\n")
	fmt.Fprintf(&b, "**Total tests:** %d\n\n", len(catalog.Tests))

	sigs := map[string]bool{}
	for _, t := range catalog.Tests {
		if t.Metadata.Sig != "" {
			sigs[t.Metadata.Sig] = true
		}
	}
	if len(sigs) > 0 {
		sl := make([]string, 0, len(sigs))
		for s := range sigs {
			sl = append(sl, s)
		}
		sort.Strings(sl)
		fmt.Fprintf(&b, "**SIGs:** %s\n\n", strings.Join(sl, ", "))
	}

	fmt.Fprintf(&b, "---\n\n")

	for i, t := range catalog.Tests {
		fmt.Fprintf(&b, "### %d. %s\n\n", i+1, t.Name)
		fmt.Fprintf(&b, "| Field | Value |\n")
		fmt.Fprintf(&b, "|-------|-------|\n")
		fmt.Fprintf(&b, "| **File** | `%s:%d` |\n", t.File, t.Line)
		fmt.Fprintf(&b, "| **Source** | %s |\n", t.Source)
		if t.Metadata.Sig != "" {
			fmt.Fprintf(&b, "| **SIG** | %s |\n", t.Metadata.Sig)
		}
		if len(t.Metadata.Features) > 0 {
			fmt.Fprintf(&b, "| **Features** | %s |\n", mdCodeList(t.Metadata.Features))
		}
		if len(t.Metadata.FeatureGates) > 0 {
			fmt.Fprintf(&b, "| **Feature Gates** | %s |\n", mdCodeList(t.Metadata.FeatureGates))
		}
		if len(t.Metadata.APIGroups) > 0 {
			fmt.Fprintf(&b, "| **API Groups** | %s |\n", mdCodeList(t.Metadata.APIGroups))
		}
		if len(t.Metadata.Suites) > 0 {
			fmt.Fprintf(&b, "| **Suites** | %s |\n", mdCodeList(t.Metadata.Suites))
		}
		if len(t.Metadata.Tags) > 0 {
			fmt.Fprintf(&b, "| **Tags** | %s |\n", mdCodeList(t.Metadata.Tags))
		}
		if t.Metadata.Timeout != "" {
			fmt.Fprintf(&b, "| **Timeout** | %s |\n", t.Metadata.Timeout)
		}
		if len(t.Metadata.Requires) > 0 {
			fmt.Fprintf(&b, "| **Requires** | %s |\n", mdCodeList(t.Metadata.Requires))
		}
		if len(t.Metadata.Skipped) > 0 {
			fmt.Fprintf(&b, "| **Skipped** | %s |\n", mdCodeList(t.Metadata.Skipped))
		}
		if t.Metadata.Jira != "" {
			fmt.Fprintf(&b, "| **Jira** | %s |\n", t.Metadata.Jira)
		}
		if t.Metadata.TestID != "" {
			fmt.Fprintf(&b, "| **Test ID** | %s |\n", t.Metadata.TestID)
		}
		if len(t.Metadata.Labels) > 0 {
			fmt.Fprintf(&b, "| **Labels** | %s |\n", mdCodeList(t.Metadata.Labels))
		}

		fmt.Fprintf(&b, "\n<details>\n<summary>Full test name</summary>\n\n")
		fmt.Fprintf(&b, "```\n%s\n```\n\n", t.FullName)
		fmt.Fprintf(&b, "</details>\n\n---\n\n")
	}

	return os.WriteFile(path, []byte(b.String()), 0644)
}

// --- Index files ---

func writeIndexYAML(catalogs map[string]*TestCatalog, dirNames []string, outputDir string) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Auto-generated test catalog index\n")
	fmt.Fprintf(&b, "# Generated by hack/testcatalog. Do not edit.\n")
	fmt.Fprintf(&b, "directories:\n")
	for _, name := range dirNames {
		catalog := catalogs[name]
		fmt.Fprintf(&b, "  - name: %s\n", yamlQuote(name))
		fmt.Fprintf(&b, "    test_count: %d\n", len(catalog.Tests))
		fmt.Fprintf(&b, "    catalog: %s\n", yamlQuote(name+".yaml"))
	}
	os.WriteFile(filepath.Join(outputDir, "index.yaml"), []byte(b.String()), 0644)
}

func writeIndexMarkdown(catalogs map[string]*TestCatalog, dirNames []string, outputDir string) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Test Catalog Index\n\n")
	fmt.Fprintf(&b, "> Auto-generated by `hack/testcatalog`. Do not edit.\n\n")

	total := 0
	for _, c := range catalogs {
		total += len(c.Tests)
	}
	fmt.Fprintf(&b, "**Total directories:** %d | **Total tests:** %d\n\n", len(catalogs), total)

	fmt.Fprintf(&b, "| Directory | Tests | Catalog |\n")
	fmt.Fprintf(&b, "|-----------|------:|--------|\n")
	for _, name := range dirNames {
		c := catalogs[name]
		fmt.Fprintf(&b, "| %s | %d | [YAML](%s.yaml) \\| [Markdown](%s.md) |\n", name, len(c.Tests), name, name)
	}
	fmt.Fprintf(&b, "\n")
	os.WriteFile(filepath.Join(outputDir, "index.md"), []byte(b.String()), 0644)
}

// --- Main ---

func main() {
	inputDir := flag.String("input", "test/extended", "Input directory containing test packages")
	outputDir := flag.String("output", "test/extended/_catalog", "Output directory for catalog files")
	format := flag.String("format", "both", "Output format: yaml, markdown, or both")
	flag.Parse()

	entries, err := os.ReadDir(*inputDir)
	if err != nil {
		log.Fatalf("error reading %s: %v", *inputDir, err)
	}

	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.Fatalf("error creating output directory %s: %v", *outputDir, err)
	}

	skipDirs := map[string]bool{
		"testdata": true,
		"util":     true,
		"scheme":   true,
		"ci":       true,
	}

	catalogs := make(map[string]*TestCatalog)

	for _, entry := range entries {
		if !entry.IsDir() || skipDirs[entry.Name()] || strings.HasPrefix(entry.Name(), "_") {
			continue
		}

		dirName := entry.Name()
		dirPath := filepath.Join(*inputDir, dirName)

		filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			relPath, _ := filepath.Rel(*inputDir, path)
			tests := extractTestsFromFile(path, relPath)
			if len(tests) > 0 {
				if _, ok := catalogs[dirName]; !ok {
					catalogs[dirName] = &TestCatalog{Directory: dirName}
				}
				catalogs[dirName].Tests = append(catalogs[dirName].Tests, tests...)
			}
			return nil
		})
	}

	dirNames := make([]string, 0, len(catalogs))
	for name := range catalogs {
		dirNames = append(dirNames, name)
	}
	sort.Strings(dirNames)

	totalTests := 0
	for _, dirName := range dirNames {
		catalog := catalogs[dirName]
		sort.Slice(catalog.Tests, func(i, j int) bool {
			if catalog.Tests[i].File != catalog.Tests[j].File {
				return catalog.Tests[i].File < catalog.Tests[j].File
			}
			return catalog.Tests[i].Line < catalog.Tests[j].Line
		})

		if *format == "yaml" || *format == "both" {
			if err := writeYAMLFile(*catalog, filepath.Join(*outputDir, dirName+".yaml")); err != nil {
				log.Printf("error writing YAML for %s: %v", dirName, err)
			}
		}
		if *format == "markdown" || *format == "both" {
			if err := writeMarkdownFile(*catalog, filepath.Join(*outputDir, dirName+".md")); err != nil {
				log.Printf("error writing markdown for %s: %v", dirName, err)
			}
		}
		totalTests += len(catalog.Tests)
	}

	if *format == "yaml" || *format == "both" {
		writeIndexYAML(catalogs, dirNames, *outputDir)
	}
	if *format == "markdown" || *format == "both" {
		writeIndexMarkdown(catalogs, dirNames, *outputDir)
	}

	fmt.Printf("Generated catalog for %d directories, %d tests total\n", len(catalogs), totalTests)
}
