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
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
)

type FrequencyMinimizer struct {
}

type FreqCandidate struct {
	matches  []int
	headways int
}

type ProgressionCover struct {
	progressions []FreqCandidate
	coveredTrips map[*gtfs.Trip]empty
}

type TripWrapper struct {
	*gtfs.Trip
	t          gtfs.Time
	marked     bool
	sourceFreq *gtfs.Frequency
}

type TripWrappers struct {
	trips        []TripWrapper
	coveredTrips map[*gtfs.Trip]empty
}

func (a TripWrappers) Len() int      { return len(a.trips) }
func (a TripWrappers) Swap(i, j int) { a.trips[i], a.trips[j] = a.trips[j], a.trips[i] }
func (a TripWrappers) Less(i, j int) bool {
	return a.trips[i].t.SecondsSinceMidnight() < a.trips[j].t.SecondsSinceMidnight()
}

/**
 * Minimize trips, stop_times and frequencies by searching optimal covers
 * for trip times.
 */
func (m FrequencyMinimizer) Run(feed *gtfsparser.Feed) {
	fmt.Fprintf(os.Stdout, "Minimizing frequencies / stop times...\n")
	processed := make(map[*gtfs.Trip]empty, 0)

	// build a slice of trips for parallel processing
	tripsSl := make([]*gtfs.Trip, 0)
	for _, t := range feed.Trips {
		tripsSl = append(tripsSl, t)
	}

	curAt := 0
	for _, t := range feed.Trips {
		curAt++
		if _, contained := processed[t]; contained {
			continue
		}

		// trips time-independent equal to the current trip
		eqs := m.getTimeIndependentEquivalentTrips(t, tripsSl, processed)
		for _, t := range eqs.trips {
			processed[t.Trip] = empty{}
		}
		if len(eqs.trips) < 2 {
			continue
		}

		var cands ProgressionCover
		var packed []ProgressionCover

		var candsOverlapping ProgressionCover
		var packedOverlapping []ProgressionCover

		cands = m.getCover(eqs, false)
		packed = m.packCovers(cands, eqs)

		candsOverlapping = m.getCover(eqs, true)
		packedOverlapping = m.packCovers(candsOverlapping, eqs)

		if len(packed) > len(packedOverlapping) {
			packed = packedOverlapping
		}

		if len(packed) >= len(eqs.coveredTrips) {
			continue
		}

		// delete now redundant trips, update service
		// each "pack" is one trip
		suffixC := 1
		for _, indProgr := range packed {
			var curTrip *gtfs.Trip = nil

			if suffixC > 1 {
				curTrip = new(gtfs.Trip)

				var newId string
				for true {
					newId = t.Id + "_" + strconv.FormatInt(int64(suffixC), 10)
					if _, in := feed.Trips[newId]; in {
						suffixC++
					} else {
						break
					}
				}

				curTrip.Id = newId
				feed.Trips[curTrip.Id] = curTrip
				processed[curTrip] = empty{}
				curTrip.Route = t.Route
				curTrip.Service = t.Service
				curTrip.Headsign = t.Headsign
				curTrip.Short_name = t.Short_name
				curTrip.Direction_id = t.Direction_id
				curTrip.Block_id = t.Block_id
				curTrip.Shape = t.Shape
				curTrip.Wheelchair_accessible = t.Wheelchair_accessible
				curTrip.Bikes_allowed = t.Bikes_allowed
				curTrip.StopTimes = make(gtfs.StopTimes, len(t.StopTimes))
				copy(curTrip.StopTimes, t.StopTimes)
			} else {
				curTrip = t
			}
			curTrip.Frequencies = make([]gtfs.Frequency, 0)

			suffixC++

			smallestStartTime := eqs.trips[indProgr.progressions[0].matches[0]].t

			// add new frequencies
			for _, p := range indProgr.progressions {
				if len(p.matches) == 1 {
					/**
					 * we can assume that progressions with 1 match are only
					 * contained in single-progression-packs
					 */
					continue
				}
				if smallestStartTime.SecondsSinceMidnight() > eqs.trips[p.matches[0]].t.SecondsSinceMidnight() {
					smallestStartTime = eqs.trips[p.matches[0]].t
				}
				a := gtfs.Frequency{}

				if eqs.trips[p.matches[0]].sourceFreq != nil {
					a.Exact_times = eqs.trips[p.matches[0]].sourceFreq.Exact_times
				} else {
					a.Exact_times = true
				}
				a.Start_time = eqs.trips[p.matches[0]].t
				a.End_time = m.getGtfsTimeFromSec(eqs.trips[p.matches[len(p.matches)-1]].t.SecondsSinceMidnight() + p.headways)
				a.Headway_secs = p.headways
				curTrip.Frequencies = append(curTrip.Frequencies, a)
			}
			m.remeasureStopTimes(curTrip, smallestStartTime)
		}

		// delete all other trips
		for _, trip := range eqs.trips {
			if trip.Id != t.Id {
				// don't delete the trip with the original id, we have used it again!
				delete(feed.Trips, trip.Id)
			}
		}
	}
}

/**
 * Pack covers into non-overlapping progressions
 */
func (m FrequencyMinimizer) packCovers(c ProgressionCover, t TripWrappers) []ProgressionCover {
	ret := make([]ProgressionCover, 0)
	singleTrips := make([]ProgressionCover, 0)
	ret = append(ret, ProgressionCover{make([]FreqCandidate, 0), make(map[*gtfs.Trip]empty, 0)})

	for _, c := range c.progressions {
		if len(c.matches) == 1 {
			// handle single-match progressions separately (they should remain single trips)
			newCover := ProgressionCover{make([]FreqCandidate, 0), make(map[*gtfs.Trip]empty, 0)}
			newCover.progressions = append(newCover.progressions, c)
			singleTrips = append(singleTrips, newCover)

			continue
		}

		// search for non-overlapping progression already on ret or insert new one
		inserted := false
		for i, existingCover := range ret {
			overlap := false
			for _, existingProg := range existingCover.progressions {
				if !(t.trips[existingProg.matches[0]].t.SecondsSinceMidnight() > t.trips[c.matches[len(c.matches)-1]].t.SecondsSinceMidnight() || t.trips[existingProg.matches[len(existingProg.matches)-1]].t.SecondsSinceMidnight() < t.trips[c.matches[0]].t.SecondsSinceMidnight()) {
					overlap = true
					break
				}
			}
			if !overlap {
				ret[i].progressions = append(ret[i].progressions, c)
				inserted = true
				break
			}
		}

		if !inserted {
			newCover := ProgressionCover{make([]FreqCandidate, 0), make(map[*gtfs.Trip]empty, 0)}
			newCover.progressions = append(newCover.progressions, c)
			ret = append(ret, newCover)
		}
	}

	if len(ret) == 1 && len(ret[0].progressions) == 0 {
		return singleTrips
	}

	return append(ret, singleTrips...)
}

/**
 * Modified version of a CAP approximation algorithm proposed by
 * Hannah Bast and Sabine Storandt in
 * http://ad-publications.informatik.uni-freiburg.de/SIGSPATIAL_frequency_BS_2014.pdf
 **/
func (m FrequencyMinimizer) getCover(eqs TripWrappers, overlapping bool) ProgressionCover {
	for i, _ := range eqs.trips {
		eqs.trips[i].marked = false
	}

	cand := ProgressionCover{make([]FreqCandidate, 0), make(map[*gtfs.Trip]empty)}
	// sort them by start time
	sort.Sort(eqs)

	// collect possible frequency values contained in this collection
	freqs := m.getPossibleFreqs(eqs)

	MINIMUM_COVER_SIZE := 2

	hasUnmarked := true
	for hasUnmarked {
		for minSize := len(eqs.trips); minSize > 0; minSize-- {
			// take the first non-marked trip and find the longest progression
			i := 0
			for ; i < len(eqs.trips)+1; i++ {
				if i >= len(eqs.trips) || !eqs.trips[i].marked {
					break
				}
			}

			if i >= len(eqs.trips) {
				// we are done for this trip
				hasUnmarked = false
				continue
			}

			startTime := eqs.trips[i].t
			curCand := FreqCandidate{make([]int, 0), 0}
			curCand.matches = append(curCand.matches, i)
			for freq, _ := range freqs {
				nextCand := FreqCandidate{make([]int, 0), 0}
				nextCand.matches = append(nextCand.matches, i)

				for j := i + 1; j < len(eqs.trips); j++ {
					if eqs.trips[j].marked {
						if overlapping {
							continue
						} else {
							break
						}
					}

					freqEq := (eqs.trips[j].sourceFreq == eqs.trips[i].sourceFreq) || (eqs.trips[j].sourceFreq == nil && eqs.trips[i].sourceFreq.Exact_times) ||
						(eqs.trips[i].sourceFreq == nil && eqs.trips[j].sourceFreq.Exact_times) || (eqs.trips[i].sourceFreq != nil && eqs.trips[j].sourceFreq != nil && eqs.trips[i].sourceFreq.Exact_times == eqs.trips[j].sourceFreq.Exact_times)
					if freqEq && eqs.trips[j].t.SecondsSinceMidnight() == (startTime.SecondsSinceMidnight())+len(nextCand.matches)*freq {
						nextCand.matches = append(nextCand.matches, j)
						nextCand.headways = freq
					} else if !overlapping {
						break
					}
				}

				if len(nextCand.matches) > len(curCand.matches) && (len(nextCand.matches) >= MINIMUM_COVER_SIZE || len(nextCand.matches) == 1) {
					curCand = nextCand
				}
			}

			// if the candidate is >= the min size, take it!
			if len(curCand.matches) >= minSize {
				cand.progressions = append(cand.progressions, curCand)
				// mark all trips as processed
				for _, t := range curCand.matches {
					eqs.trips[t].marked = true
					cand.coveredTrips[eqs.trips[t].Trip] = empty{}
				}
			}
		}
	}
	return cand
}

/**
 * Get possible frequencies from a collection of TripWrappers
 */
func (m FrequencyMinimizer) getPossibleFreqs(tws TripWrappers) map[int]empty {
	ret := make(map[int]empty, 0)

	for i, _ := range tws.trips {
		for ii := i + 1; ii < len(tws.trips); ii++ {
			fre := tws.trips[ii].t.SecondsSinceMidnight() - tws.trips[i].t.SecondsSinceMidnight()
			if fre != 0 {
				ret[fre] = empty{}
			}
		}
	}
	return ret
}

/**
 * Get trips that are equal to trip without considering the absolute time values
 */
func (m FrequencyMinimizer) getTimeIndependentEquivalentTrips(trip *gtfs.Trip, trips []*gtfs.Trip, processed map[*gtfs.Trip]empty) TripWrappers {
	ret := TripWrappers{make([]TripWrapper, 0), make(map[*gtfs.Trip]empty, 0)}

	chunks := MaxParallelism()
	sem := make(chan empty, chunks)
	workload := int(math.Ceil(float64(len(trips)) / float64(chunks)))
	mutex := &sync.Mutex{}

	for j := 0; j < chunks; j++ {
		go func(j int) {
			for i := workload * j; i < workload*(j+1) && i < len(trips); i++ {
				t := trips[i]

				if t.Id == trip.Id || m.isTimeIndependentEqual(t, trip) {
					if len(t.Frequencies) == 0 {
						mutex.Lock()
						ret.trips = append(ret.trips, TripWrapper{t, t.StopTimes[0].Arrival_time, false, nil})
						ret.coveredTrips[t] = empty{}
						mutex.Unlock()
					} else {
						// expand frequencies
						for _, f := range t.Frequencies {
							for s := f.Start_time.SecondsSinceMidnight(); s < f.End_time.SecondsSinceMidnight(); s = s + f.Headway_secs {
								mutex.Lock()
								ret.trips = append(ret.trips, TripWrapper{t, m.getGtfsTimeFromSec(s), false, &f})
								ret.coveredTrips[t] = empty{}
								mutex.Unlock()
							}
						}
					}
				}
			}
			sem <- empty{}
		}(j)
	}

	for i := 0; i < chunks; i++ {
		<-sem
	}
	return ret
}

/**
 * Convert seconds since midnight to a GTFS time
 */
func (m FrequencyMinimizer) getGtfsTimeFromSec(s int) gtfs.Time {
	return gtfs.Time{s / 3600, int8((s - (s/3600)*3600) / 60), int8(s - ((s / 60) * 60))}
}

/**
 * Check if two trips are equal without considering absolute stop times
 */
func (m FrequencyMinimizer) isTimeIndependentEqual(a *gtfs.Trip, b *gtfs.Trip) bool {
	return a.Route == b.Route && a.Service == b.Service && a.Headsign == b.Headsign &&
		a.Short_name == b.Short_name && a.Direction_id == b.Direction_id && a.Block_id == b.Block_id &&
		a.Shape == b.Shape && a.Wheelchair_accessible == b.Wheelchair_accessible &&
		a.Bikes_allowed == b.Bikes_allowed && m.hasSameRelStopTimes(a, b)
}

/**
 * Remeasure a trips stop times by taking their relative values and changing the sequence to
 * start with time
 */
func (m FrequencyMinimizer) remeasureStopTimes(t *gtfs.Trip, time gtfs.Time) {
	diff := 0
	curArrDepDiff := 0
	for i := 0; i < len(t.StopTimes); i++ {
		curArrDepDiff = t.StopTimes[i].Departure_time.SecondsSinceMidnight() - t.StopTimes[i].Arrival_time.SecondsSinceMidnight()
		oldArrT := t.StopTimes[i].Arrival_time.SecondsSinceMidnight()

		t.StopTimes[i].Arrival_time = m.getGtfsTimeFromSec(time.SecondsSinceMidnight() + diff)
		t.StopTimes[i].Departure_time = m.getGtfsTimeFromSec(time.SecondsSinceMidnight() + diff + curArrDepDiff)

		if i < len(t.StopTimes)-1 {
			diff += t.StopTimes[i+1].Arrival_time.SecondsSinceMidnight() - oldArrT
		}
	}
}

/**
 * true if two trips share the same stops in the same order with the same
 * relative stop times
 */
func (m FrequencyMinimizer) hasSameRelStopTimes(a *gtfs.Trip, b *gtfs.Trip) bool {
	// handle trivial cases
	if len(a.StopTimes) != len(b.StopTimes) {
		return false
	}

	if len(a.StopTimes) == 0 && len(b.StopTimes) == 0 {
		return true
	}

	var aPrev *gtfs.StopTime
	var bPrev *gtfs.StopTime

	for i, _ := range a.StopTimes {
		if !(a.StopTimes[i].Stop == b.StopTimes[i].Stop &&
			a.StopTimes[i].Headsign == b.StopTimes[i].Headsign &&
			a.StopTimes[i].Pickup_type == b.StopTimes[i].Pickup_type && a.StopTimes[i].Drop_off_type == b.StopTimes[i].Drop_off_type &&
			floatEquals(a.StopTimes[i].Shape_dist_traveled, b.StopTimes[i].Shape_dist_traveled, 0.01) && a.StopTimes[i].Timepoint == b.StopTimes[i].Timepoint) {
			return false
		}
		if i != 0 {
			if a.StopTimes[i].Arrival_time.SecondsSinceMidnight()-aPrev.Arrival_time.SecondsSinceMidnight() != b.StopTimes[i].Arrival_time.SecondsSinceMidnight()-bPrev.Arrival_time.SecondsSinceMidnight() {
				return false
			}
			if a.StopTimes[i].Departure_time.SecondsSinceMidnight()-aPrev.Departure_time.SecondsSinceMidnight() != b.StopTimes[i].Departure_time.SecondsSinceMidnight()-bPrev.Departure_time.SecondsSinceMidnight() {
				return false
			}
		}

		aPrev = &a.StopTimes[i]
		bPrev = &b.StopTimes[i]
	}
	return true
}

func floatEquals(a float32, b float32, e float32) bool {
	if (a-b) < e && (b-a) < e {
		return true
	}
	return false
}

func MaxParallelism() int {
	maxProcs := runtime.GOMAXPROCS(0)
	numCPU := runtime.NumCPU()
	if maxProcs < numCPU {
		return maxProcs
	}
	return numCPU
}