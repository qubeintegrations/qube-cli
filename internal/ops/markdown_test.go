package ops

import (
	"reflect"
	"strings"
	"testing"
)

func TestRenderInline(t *testing.T) {
	for in, want := range map[string]string{
		"qbXML: `InvoiceAddRq`.":                                  "qbXML: InvoiceAddRq.",
		"see [Modify, delete](https://intuit.example/mod) first.": "see Modify, delete (https://intuit.example/mod) first.",
		"[https://a.example/x](https://a.example/x)":              "https://a.example/x",
		"[`code` text](https://a.example/y \"title\")":            "code text (https://a.example/y)",
		"<https://a.example/z>":                                   "https://a.example/z",
		"**Note:** read *this*.":                                  "Note: read this.",
		`Amount = (Quantity \* Rate) + markup`:                    "Amount = (Quantity * Rate) + markup",
		`support \*and\* more`:                                    "support *and* more",
		"a*b*c and x * y and snake_case_name":                     "a*b*c and x * y and snake_case_name",
		"`<hostqueryrq></hostqueryrq>` (no space)":                "<hostqueryrq></hostqueryrq> (no space)",
		"an `unclosed tick":                                       "an `unclosed tick",
		"[not a link] (here)":                                     "[not a link] (here)",
		"``a ` b``":                                               "a ` b",
	} {
		if got := renderInline(in); got != want {
			t.Errorf("renderInline(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderMarkdownBlocks(t *testing.T) {
	md := "### The Life Cycle\n\nIntro with `code`.\n\n" +
		"- Let’s say we bought 10 widgets for $100. QuickBooks would then consider each widget to be worth $10.\n" +
		"- The next day a customer buys 5.\n\n" +
		"1. To record a payment,\n2. To set a discount.\n\n" +
		"> 1% 10 Net 30\n\n" +
		"Within EstimateLineMod:\n\n- ItemRef\n- Amount\n\n" +
		"For example:\n\n    indented code\n"
	want := strings.Join([]string{
		"The Life Cycle",
		"",
		"Intro with code.",
		"",
		"- Let’s say we bought 10 widgets for $100. QuickBooks would",
		"  then consider each widget to be worth $10.",
		"- The next day a customer buys 5.",
		"",
		"1. To record a payment,",
		"2. To set a discount.",
		"",
		"    1% 10 Net 30",
		"",
		"Within EstimateLineMod:",
		"",
		"- ItemRef",
		"- Amount",
		"",
		"For example:",
		"",
		"    indented code",
	}, "\n")
	if got := renderMarkdown(md, 60); got != want {
		t.Fatalf("renderMarkdown:\n%s\nwant:\n%s", got, want)
	}
}

func TestWrapNeverBreaksAURL(t *testing.T) {
	url := "https://developer.intuit.com/app/developer/qbdesktop/docs/develop/exploring-the-quickbooks-desktop-sdk/modify-delete-void"
	got := renderMarkdown("For more details, see [Modify requests]("+url+") in the guide.", 40)
	if !strings.Contains(got, "\n("+url+")") {
		t.Fatalf("the URL should stand whole on its own line:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 40 && !strings.Contains(line, url) {
			t.Errorf("line over the width: %q", line)
		}
	}
}

func TestSentencesKeepLinksAndCode(t *testing.T) {
	got := sentences("See [Modify. Delete.](https://x.example/a.b). Then `a. b` too, e.g. this one! Last “quoted.” End")
	want := []string{"See [Modify. Delete.](https://x.example/a.b).", "Then `a. b` too, e.g. this one!", "Last “quoted.”", "End"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sentences = %q\nwant        %q", got, want)
	}
}

func TestBriefNeverCutsInsideALink(t *testing.T) {
	link := "[Modify, delete, and void requests and responses](https://developer.intuit.com/app/developer/qbdesktop/docs/develop/exploring-the-quickbooks-desktop-sdk/modify-delete-void)"
	md := strings.Repeat("Words go here. ", 30) + "\n\n(For more details, see " + link + ".) More after it."
	for max := 400; max < len(md); max += 7 {
		got, cut := brief(md, max)
		if !cut {
			t.Fatalf("max %d: nothing cut", max)
		}
		if strings.Contains(got, "[Modify") != strings.Contains(got, "modify-delete-void)") {
			t.Fatalf("max %d: a link cut in two: %q", max, got[len(got)-80:])
		}
	}
}

func TestBriefCutsBetweenItemsAndDropsADanglingHeading(t *testing.T) {
	items := "- " + strings.Repeat("x", 60) + "\n- " + strings.Repeat("y", 60) + "\n- " + strings.Repeat("z", 60)
	got, cut := brief("Intro.\n\n"+items, 140)
	if !cut || got != "Intro.\n\n- "+strings.Repeat("x", 60)+"\n- "+strings.Repeat("y", 60) {
		t.Fatalf("brief = %q (cut %v)", got, cut)
	}
	got, cut = brief("Intro.\n\n### A heading\n\n"+strings.Repeat("A long sentence that will not fit at all ", 20), 200)
	if !cut || got != "Intro." {
		t.Fatalf("a heading whose section is cut should go too: %q", got)
	}
}

func TestDocSentencePassesOverGeneratedNotesWhenHinted(t *testing.T) {
	note := "Choose at most one of: `rate`, or `rate_percent`."
	if got := docSentence(note, true); got != "" {
		t.Fatalf("a note the hint restates should go: %q", got)
	}
	if got := docSentence("The price. More.\n\n"+note, true); got != "The price." {
		t.Fatalf("docSentence = %q", got)
	}
	if got := docSentence(note, false); got != "Choose at most one of: rate, or rate_percent." {
		t.Fatalf("without a hint the note stays: %q", got)
	}
}
