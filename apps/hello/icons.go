package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

const iconColor = "#2f6f4f"

// makeIcons draws the home-screen icons: a white note card on green.
func makeIcons() map[string][]byte {
	return map[string][]byte{
		"apple-touch-icon.png": drawIcon(180),
		"icon-192.png":         drawIcon(192),
		"icon-512.png":         drawIcon(512),
	}
}

func drawIcon(size int) []byte {
	bg := color.RGBA{0x2f, 0x6f, 0x4f, 0xff}
	card := color.RGBA{0xfa, 0xf8, 0xf2, 0xff}
	line := color.RGBA{0x2f, 0x6f, 0x4f, 0xff}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x), float64(y)
			c := bg
			if fx > s*0.24 && fx < s*0.76 && fy > s*0.2 && fy < s*0.8 {
				c = card
				for _, ly := range []float64{0.36, 0.48, 0.60} {
					if fy > s*ly && fy < s*(ly+0.045) && fx > s*0.32 && fx < s*(0.68-boolF(ly == 0.60)*0.12) {
						c = line
					}
				}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
