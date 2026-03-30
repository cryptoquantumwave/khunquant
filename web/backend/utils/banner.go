package utils

import "github.com/khunquant/khunquant/pkg/brand"

// Banner is the KHUNQUANT ASCII art banner for the web launcher startup output.
var Banner = "\r\n" + brand.Stacked(brand.ANSIBlue, brand.ANSIRed, brand.ANSIReset)
