package model

import (
	"fmt"
	"math"
)

// DeepSeek's pinned v41 image geometry. Inputs are normalized attachment
// dimensions; pricing must use the dimensions actually sent to the provider.
type deepSeekGrid struct{ height, width, pixelHeight, pixelWidth, tokens int }

func gridCells(pixels int) int { return ((pixels / 14) + 2) / 3 }
func gridTokens(h, w int) int  { return h*(w+1) + 2 }

func solveImageGrid(height, width int) deepSeekGrid {
	aspect := float64(height) / float64(width)
	idealWidth := math.Sqrt(1022/aspect+0.25) - 0.5
	idealHeight := idealWidth * aspect
	var bestHeight, bestWidth int
	switch {
	case idealWidth < 1:
		bestWidth = 42
		bestHeight = (1022 / 2) * 42
	case idealHeight < 1:
		bestHeight = 42
		bestWidth = 1021 * 42
	default:
		w, h := int(idealWidth), int(idealHeight)
		scale := math.Min(float64(w*42)/float64(width), float64(h*42)/float64(height))
		bestWidth = int(float64(width)*scale/14) * 14
		bestHeight = int(float64(height)*scale/14) * 14
	}
	h, w := gridCells(bestHeight), gridCells(bestWidth)
	return deepSeekGrid{h, w, bestHeight, bestWidth, gridTokens(h, w)}
}

func resizeImageGrid(width, height int) deepSeekGrid {
	w, h := width, height
	if pixels := float64(w) * float64(h); pixels < 544*544 {
		scale := math.Sqrt((544 * 544) / pixels)
		w, h = int(float64(w)*scale), int(float64(h)*scale)
	}
	pw, ph := ((w+13)/14)*14, ((h+13)/14)*14
	gw, gh := gridCells(pw), gridCells(ph)
	if tokens := gridTokens(gh, gw); tokens <= 1024 {
		return deepSeekGrid{gh, gw, ph, pw, tokens}
	}
	return solveImageGrid(h, w)
}

func validImageGeometry(width, height int) error {
	if width < 1 || height < 1 || width > 8192 || height > 8192 {
		return fmt.Errorf("invalid normalized image dimensions %dx%d", width, height)
	}
	return nil
}

// DeepSeekRequestImageDimensions preserves the whole source within the
// provider token grid and per-side cap. It never enlarges a source image.
func DeepSeekRequestImageDimensions(width, height int) (int, int, error) {
	if err := validImageGeometry(width, height); err != nil {
		return 0, 0, err
	}
	w, h := width, height
	pw, ph := ((width+13)/14)*14, ((height+13)/14)*14
	if gridTokens(gridCells(ph), gridCells(pw)) > 1024 {
		solved := solveImageGrid(height, width)
		if width >= height {
			w = solved.pixelWidth
			h = max(1, int(math.Floor(float64(w)*float64(height)/float64(width)+0.5)))
		} else {
			h = solved.pixelHeight
			w = max(1, int(math.Floor(float64(h)*float64(width)/float64(height)+0.5)))
		}
	}
	if max(w, h) > 4096 {
		if width >= height {
			w = 4096
			h = max(1, int(math.Floor(4096*float64(height)/float64(width)+0.5)))
		} else {
			h = 4096
			w = max(1, int(math.Floor(4096*float64(width)/float64(height)+0.5)))
		}
	}
	return w, h, nil
}

func DeepSeekImageTokens(width, height int) (int, error) {
	if err := validImageGeometry(width, height); err != nil {
		return 0, err
	}
	result := resizeImageGrid(width, height)
	for iteration := 1; iteration < 10; iteration++ {
		next := resizeImageGrid(result.pixelWidth, result.pixelHeight)
		if next == result {
			return result.tokens, nil
		}
		result = next
	}
	return 0, fmt.Errorf("image token resize did not converge for %dx%d", width, height)
}
