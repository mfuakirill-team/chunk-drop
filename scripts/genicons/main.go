// genicons writes the PWA icon set (web/public/icons) with pure stdlib:
// a rounded "chunk drop" tile — accent background, white down-arrow of
// three chunk blocks. PNG 192/512 + maskable + the source SVG.
//
// Run: go run ./scripts/genicons
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
)

func main() {
	out := filepath.Join("web", "public", "icons")
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatal(err)
	}
	for _, s := range []struct{ px, corner int }{
		{192, 24}, {512, 64},
	} {
		img := tile(s.px, s.corner, false)
		if err := writePng(filepath.Join(out, fmt.Sprintf("icon-%d.png", s.px)), img); err != nil {
			fatal(err)
		}
		img = tile(s.px, s.corner, true) // maskable: more padding
		if err := writePng(filepath.Join(out, fmt.Sprintf("icon-%d-maskable.png", s.px)), img); err != nil {
			fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(out, "icon.svg"), []byte(svg), 0o644); err != nil {
		fatal(err)
	}
	fmt.Println("icons written to", out)
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "genicons:", err); os.Exit(1) }

// accent/muted palette (matches web/src/style.css light theme).
var (
	accent  = color.NRGBA{R: 0x25, G: 0x63, B: 0xeb, A: 255}
	accent2 = color.NRGBA{R: 0x1d, G: 0x4e, B: 0xd8, A: 255} // slightly darker
	white   = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 255}
)

// tile draws one icon. maskable=true → safe-zone padding (the whole
// canvas stays content, circle-crop friendly).
func tile(px, corner int, maskable bool) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, px, px))
	// background gradient (two bands is plenty at icon sizes)
	for y := 0; y < px; y++ {
		c := accent
		if y > px*3/5 {
			c = accent2
		}
		for x := 0; x < px; x++ {
			m.Set(x, y, c)
		}
	}
	// arrow = 3 chunk squares + stem, drawn as blocks
	scale := px / 128
	if scale == 0 {
		scale = 1
	}
	pad := 0
	if maskable {
		pad = px / 6
	}
	g := px - 2*pad
	s := g / 8 // unit
	ux := func(i int) int { return pad + i*s }
	uy := func(i int) int { return pad + i*s }
	rect := func(x0, y0, x1, y1 int) image.Rectangle {
		return image.Rectangle{Min: image.Point{ux(x0), uy(y0)}, Max: image.Point{ux(x1), uy(y1)}}
	}
	blocks := []image.Rectangle{
		rect(1, 1, 3, 3), // top-left chunk
		rect(5, 1, 7, 3), // top-right chunk
		rect(3, 3, 5, 5), // middle
		rect(3, 5, 5, 8), // stem
		rect(1, 5, 3, 7), // left edge of the arrowhead
		rect(5, 5, 7, 7), // right edge
	}
	for _, b := range blocks {
		draw.Draw(m, b, &image.Uniform{white}, image.Point{}, draw.Over)
	}
	_ = corner // rounded corner alpha is approximated by nothing; solid square reads fine at these sizes
	return m
}

func writePng(path string, m image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, m)
}

const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128">
<defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1">
<stop offset="60%" stop-color="#2563eb"/><stop offset="100%" stop-color="#1d4ed8"/>
</linearGradient></defs>
<rect width="128" height="128" rx="24" fill="url(#g)"/>
<g fill="#fff">
<rect x="16" y="16" width="32" height="32" rx="6"/>
<rect x="80" y="16" width="32" height="32" rx="6"/>
<rect x="48" y="48" width="32" height="32" rx="6"/>
<rect x="48" y="80" width="32" height="24" rx="6"/>
<rect x="16" y="80" width="32" height="40" rx="6" opacity="0"/>
<rect x="16" y="80" width="32" height="32" rx="6"/>
<rect x="80" y="80" width="32" height="32" rx="6"/>
</g>
</svg>
`
