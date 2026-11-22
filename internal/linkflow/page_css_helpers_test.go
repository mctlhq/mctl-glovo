package linkflow

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var declRe = regexp.MustCompile(`(--[a-z-]+):\s*(#[0-9a-fA-F]{6})`)

// cssVars reads the colour variables of the light block (:root) and, for
// dark, overlays those of the prefers-color-scheme:dark block.
func cssVars(t *testing.T, css string, dark bool) map[string]string {
	t.Helper()
	i := strings.Index(css, "@media (prefers-color-scheme:dark)")
	if i < 0 {
		t.Fatal("no dark block")
	}
	m := map[string]string{}
	for _, d := range declRe.FindAllStringSubmatch(css[:i], -1) {
		m[d[1]] = d[2]
	}
	if dark {
		end := strings.Index(css[i:], "\n}\n")
		for _, d := range declRe.FindAllStringSubmatch(css[i:i+end], -1) {
			m[d[1]] = d[2]
		}
	}
	return m
}

func hexRGB(t *testing.T, h string) [3]float64 {
	t.Helper()
	if len(h) != 7 {
		t.Fatalf("not a colour: %q", h)
	}
	var c [3]float64
	for i := range c {
		v, err := strconv.ParseUint(h[1+2*i:3+2*i], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		c[i] = float64(v) / 255
	}
	return c
}

func relLum(c [3]float64) float64 {
	f := func(x float64) float64 {
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*f(c[0]) + 0.7152*f(c[1]) + 0.0722*f(c[2])
}

func contrast(a, b [3]float64) float64 {
	la, lb := relLum(a), relLum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
