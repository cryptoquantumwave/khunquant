// Package brand holds the canonical KHUNQUANT ASCII art banner.
// Edit the line arrays here to change the banner everywhere.
package brand

import "strings"

// ANSI color codes used by terminal consumers.
const (
	ANSIBlue  = "\033[1;38;2;62;93;185m"   // #3e5db9
	ANSIRed   = "\033[1;38;2;255;255;255m" // #ffffff  White
	ANSIReset = "\033[0m"
)

// blueLines is the KHUN half of the banner (4 rows).
var blueLines = [4]string{
	"██╗  ██╗██╗  ██╗██╗   ██╗███╗   ██╗",
	"█████╔╝ ███████║██║   ██║██╔██╗ ██║",
	"██╔═██╗ ██╔══██║╚██████╔╝██║╚██╗██║",
	"╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═══╝",
}

// redLines is the QUANT half of the banner (4 rows).
var redLines = [4]string{
	" ██████╗ ██╗   ██╗ █████╗ ███╗   ██╗████████╗",
	"██║   ██║██║   ██║███████║██╔██╗ ██║╚══██╔══╝",
	"██║▄▄ ██║╚██████╔╝██╔══██║██║╚██╗██║   ██║   ",
	" ╚══▀▀═╝  ╚═════╝ ╚═╝  ╚═╝╚═╝  ╚═══╝   ╚═╝   ",
}

// SideBySide builds a 4-row banner where each row shows the blue (KHUN) half
// and red (QUANT) half on the same line. Suitable for standard terminals.
//
//	brand.SideBySide(brand.ANSIBlue, brand.ANSIRed, brand.ANSIReset)
//	brand.SideBySide("[#3e5db9]", "[#ffffff]", "[::]")  // tview
func SideBySide(blue, red, reset string) string {
	var sb strings.Builder
	for i := range blueLines {
		sb.WriteString(blue)
		sb.WriteString(blueLines[i])
		sb.WriteString(red)
		sb.WriteString(redLines[i])
		sb.WriteByte('\n')
	}
	sb.WriteString(reset)
	return sb.String()
}

// Stacked builds an 8-row banner where the blue (KHUN) block appears above the
// red (QUANT) block. Suitable for wider display areas (e.g. web launcher).
//
//	brand.Stacked(brand.ANSIBlue, brand.ANSIRed, brand.ANSIReset)
func Stacked(blue, red, reset string) string {
	var sb strings.Builder
	for _, l := range blueLines {
		sb.WriteString(blue)
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	for _, l := range redLines {
		sb.WriteString(red)
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	sb.WriteString(reset)
	return sb.String()
}
