// Copyright 2016 Patrick Brosi
// Authors: info@patrickbrosi.de
//
// Use of this source code is governed by a GPL v2
// license that can be found in the LICENSE file

package processors

import (
	"fmt"
	"github.com/patrickbr/gtfsparser"
	gtfs "github.com/patrickbr/gtfsparser/gtfs"
	"os"
)

type ShapeRemeasurer struct {
	ShapeMinimizer
}

/*
 * Remeasure shapes
 */
func (s ShapeRemeasurer) Run(feed *gtfsparser.Feed) {
	fmt.Fprintf(os.Stdout, "Remeasuring shapes...\n")
	for _, shp := range feed.Shapes {
		s.remeasure(shp)
	}
}

/*
 * Remeasure a single shape
 */
func (s ShapeRemeasurer) remeasure(shape *gtfs.Shape) {
	avgMeasure, nulled := s.remasureKnown(shape)

	if !nulled {
		s.remasureUnknown(shape, avgMeasure)
	} else {
		// no avg measurement found, null all values
		for i, _ := range shape.Points {
			shape.Points[i].Dist_traveled = 0
			shape.Points[i].Has_dist = false
		}
	}
}

/*
 * Remeasure parts of the shape we could not guess the correct measurement by using
 * the average measurement
 */
func (s ShapeRemeasurer) remasureUnknown(shape *gtfs.Shape, avgMeasure float64) {
	lastUMIndex := -1
	lastM := 0.0

	for i := 0; i <= len(shape.Points); i++ {
		if i == len(shape.Points) || shape.Points[i].HasDistanceTraveled() {
			if lastUMIndex > -1 {
				s.remeasureBetween(lastUMIndex, i, avgMeasure, lastM, shape)
				lastUMIndex = -1
			}
			if i < len(shape.Points) {
				lastM = float64(shape.Points[i].Dist_traveled)
			}
		} else if lastUMIndex == -1 {
			lastUMIndex = i
		}
	}
}

/*
 * Remeasure parts of the shape we can guess by using surrounding points
 */
func (s ShapeRemeasurer) remasureKnown(shape *gtfs.Shape) (float64, bool) {
	c := 0
	m := 0.0

	lastMIndex := -1
	lastM := -1.0
	hasLast := false
	d := 0.0

	for i := 0; i < len(shape.Points); i++ {
		if i > 0 {
			d = d + s.distP(&shape.Points[i-1], &shape.Points[i])
		}
		if shape.Points[i].HasDistanceTraveled() {
			if hasLast && d > 0 {
				localM := (float64(shape.Points[i].Dist_traveled) - lastM) / d

				if i-lastMIndex > 1 {
					s.remeasureBetween(lastMIndex+1, i, localM, lastM, shape)
				}
				m = m + localM
				c++
			}

			lastMIndex = i
			lastM = float64(shape.Points[i].Dist_traveled)
			hasLast = shape.Points[i].HasDistanceTraveled()
			d = 0
		}
	}

	if c == 0 {
		return 0, true
	}
	return m / float64(c), false
}

/*
 * Remeasure between points i and end
 */
func (s ShapeRemeasurer) remeasureBetween(i int, end int, mPUnit float64, lastMeasure float64, shape *gtfs.Shape) {
	d := 0.0

	for ; i < end; i++ {
		if i > 0 {
			d = d + s.distP(&shape.Points[i-1], &shape.Points[i])
		}
		shape.Points[i].Dist_traveled = float32(lastMeasure) + float32(d*mPUnit)
		shape.Points[i].Has_dist = true
	}
}