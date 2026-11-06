package commands

import "testing"

func TestParse(t *testing.T) {
	one := []string{"Pixel 7 Pro"}
	two := []string{"Pixel 7 Pro", "Mom's phone"}
	cases := []struct {
		text string
		devs []string
		want Command
	}{
		{"Send this to my phone", one, Command{Kind: Send, What: This, Device: "Pixel 7 Pro"}},
		{"send this to pixel", two, Command{Kind: Send, What: This, Device: "Pixel 7 Pro"}},
		{"send the latest download to my phone in documents", one, Command{Kind: Send, What: Download, Device: "Pixel 7 Pro", Folder: "documents"}},
		{"send my latest screenshot to downloads on pixel", two, Command{Kind: Send, What: Screenshot, Device: "Pixel 7 Pro", Folder: "downloads"}},
		{"can you share report dot pdf with my phone", one, Command{Kind: Send, What: Named, Name: "report.pdf", Device: "Pixel 7 Pro"}},
		{"send the last video to my phone", two, Command{Kind: Send, What: Video}},
		{"send this", one, Command{Kind: Send, What: This}},
		{"get budget.xlsx from my phone", one, Command{Kind: Get, What: Named, Name: "budget.xlsx", Device: "Pixel 7 Pro"}},
		{"get the latest photo from my phone", one, Command{Kind: Get, What: Photo, Device: "Pixel 7 Pro"}},
		{"bring my last screenshot from pixel", two, Command{Kind: Get, What: Screenshot, Device: "Pixel 7 Pro"}},
		{"download the holiday photos", two, Command{Kind: Get, What: Photo}},
		{"is mom's phone online?", two, Command{Kind: Status, Device: "Mom's phone"}},
		{"send my clipboard to my phone", one, Command{Kind: Send, What: Clipboard, Device: "Pixel 7 Pro"}},
		{"send what I copied to mom's phone", two, Command{Kind: Send, What: Clipboard, Device: "Mom's phone"}},
		{"paste from my phone", one, Command{Kind: Paste, Device: "Pixel 7 Pro"}},
		{"get the clipboard from pixel", two, Command{Kind: Paste, Device: "Pixel 7 Pro"}},
		{"paste", two, Command{Kind: Paste}},
		{"what's the weather", one, Command{}},
	}
	for _, c := range cases {
		if got := Parse(c.text, c.devs); got != c.want {
			t.Errorf("%q:\n got  %+v\n want %+v", c.text, got, c.want)
		}
	}
}

func TestFolders(t *testing.T) {
	labels := []string{"Downloads", "Documents", "Pictures", "Work"}
	for in, want := range map[string]string{"documents": "Documents", "download": "Downloads", "work": "Work", "d": "", "pics": ""} {
		if got := FindFolder(in, labels); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestNames(t *testing.T) {
	if NameScore("Report.pdf", "report.pdf") != 100 || NameScore("Quarterly report 2026.pdf", "quarterly report") == 0 || NameScore("notes.txt", "budget") != 0 {
		t.Error("name scores")
	}
}
