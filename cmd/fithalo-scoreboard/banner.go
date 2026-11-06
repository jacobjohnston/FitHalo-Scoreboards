package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	// styleBold is the ANSI sequence for the banner's heading.
	styleBold = "\x1b[1m"

	// styleCode is the ANSI sequence for the setup code, bold bright cyan.
	styleCode = "\x1b[1;96m"

	// styleDim is the ANSI sequence for the box's border.
	styleDim = "\x1b[2m"

	// styleLink is the ANSI sequence for the setup URL, underlined cyan.
	styleLink = "\x1b[4;36m"

	// styleReset is the ANSI sequence that ends a style.
	styleReset = "\x1b[0m"

	// bannerInset indents the box from the terminal's edge.
	bannerInset = "  "

	// setupCodeVariable names a configured setup code, which the banner never prints.
	setupCodeVariable = "SCOREBOARD_SETUP_CODE"
)

// bannerLine is one line of the setup banner, as plain text and as it is styled on a terminal.
type bannerLine struct {
	// plain is the line without escape sequences, which sets its width.
	plain string

	// styled is the line as written, with any escape sequences.
	styled string
}

// writeSetupBanner says how to create the admin: on a terminal, a box with the setup code on its own
// line; anywhere else, such as journald or CloudWatch, plain lines a log keeps whole. An empty code
// means the configured one, which is named rather than written.
func writeSetupBanner(writer io.Writer, setupURL, code string, terminal, colour bool) {
	if !terminal {
		shown := code
		if code == "" {
			shown = "the one set in " + setupCodeVariable
		}
		fmt.Fprintf(writer, "FitHalo Scoreboard has no admin yet.\n  Set it up at: %s\n  Setup code:   %s\n", setupURL, shown)
		return
	}

	style := func(sequence, text string) string {
		if !colour {
			return text
		}
		return sequence + text + styleReset
	}
	lines := []bannerLine{
		{},
		{"FitHalo Scoreboard has no admin yet.", style(styleBold, "FitHalo Scoreboard has no admin yet.")},
		{},
		{"1. Open   " + setupURL, "1. Open   " + style(styleLink, setupURL)},
	}
	if code == "" {
		configured := "2. Enter the setup code set in " + setupCodeVariable + "."
		lines = append(lines, bannerLine{configured, configured}, bannerLine{})
	} else {
		lines = append(lines, bannerLine{"2. Enter this setup code:", "2. Enter this setup code:"}, bannerLine{},
			bannerLine{"     " + code, "     " + style(styleCode, code)}, bannerLine{})
	}
	width := 0
	for _, line := range lines {
		width = max(width, utf8.RuneCountInString(line.plain))
	}
	border := strings.Repeat("─", width+4)
	var output strings.Builder
	output.WriteString("\n" + bannerInset + style(styleDim, "╭"+border+"╮") + "\n")
	for _, line := range lines {
		padding := strings.Repeat(" ", width-utf8.RuneCountInString(line.plain))
		output.WriteString(bannerInset + style(styleDim, "│") + "  " + line.styled + padding + "  " + style(styleDim, "│") + "\n")
	}
	output.WriteString(bannerInset + style(styleDim, "╰"+border+"╯") + "\n\n")
	io.WriteString(writer, output.String())
}

// isTerminal reports whether a file is an interactive terminal rather than a pipe, a file or a log.
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// wantsColour reports whether a terminal should get colour, honouring NO_COLOR (no-color.org).
func wantsColour() bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}
