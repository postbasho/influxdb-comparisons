package main

import (
	"fmt"
	"time"
)

// HLQuery is a high-level query, usually read from stdin after being
// generated by a bulk query generator program.
//
// The primary use of an HLQuery is to combine it with a ClientSideIndex to
// construct a QueryPlan.
type HLQuery struct {
	HumanLabel       []byte
	HumanDescription []byte
	ID               int64

	MeasurementName []byte // e.g. "cpu"
	FieldName       []byte // e.g. "usage_user"
	AggregationType []byte // e.g. "avg" or "sum". used literally in the cassandra query.
	TimeStart       time.Time
	TimeEnd         time.Time
	GroupByDuration time.Duration
	TagSets         [][]string // semantically, each subgroup is OR'ed and they are all AND'ed together
}

// String produces a debug-ready description of a Query.
func (q *HLQuery) String() string {
	return fmt.Sprintf("ID: %d, HumanLabel: %s, HumanDescription: %s, MeasurementName: %s, FieldName: %s, AggregationType: %s, TimeStart: %s, TimeEnd: %s, GroupByDuration: %s, TagSets: %s", q.ID, q.HumanLabel, q.HumanDescription, q.MeasurementName, q.FieldName, q.AggregationType, q.TimeStart, q.TimeEnd, q.GroupByDuration, q.TagSets)
}

// ForceUTC rewrites timestamps in UTC, which is helpful for pretty-printing.
func (q *HLQuery) ForceUTC() {
	q.TimeStart = q.TimeStart.UTC()
	q.TimeEnd = q.TimeEnd.UTC()
}

// ToQueryPlanWithServerAggregation combines an HLQuery with a
// ClientSideIndex to make a QueryPlanWithServerAggregation.
func (q *HLQuery) ToQueryPlanWithServerAggregation(csi *ClientSideIndex) (qp *QueryPlanWithServerAggregation, err error) {
	seriesChoices := csi.SeriesForMeasurementAndField(string(q.MeasurementName), string(q.FieldName))

	// Build the time buckets used for 'group by time'-type queries.
	//
	// It is important to populate these even if they end up being empty,
	// so that we get correct results for empty 'time buckets'.
	tis := bucketTimeIntervals(q.TimeStart, q.TimeEnd, q.GroupByDuration)
	bucketedSeries := map[TimeInterval][]Series{}
	for _, ti := range tis {
		bucketedSeries[ti] = []Series{}
	}

	// For each known db series, associate it to its applicable time
	// buckets, if any:
	for _, s := range seriesChoices {
		// quick skip if the series doesn't match at all:
		if !s.MatchesMeasurementName(string(q.MeasurementName)) {
			continue
		}
		if !s.MatchesFieldName(string(q.FieldName)) {
			continue
		}
		if !s.MatchesTagSets(q.TagSets) {
			continue
		}

		// check each group-by interval to see if it applies:
		for _, ti := range tis {
			if !s.MatchesTimeInterval(&ti) {
				continue
			}
			bucketedSeries[ti] = append(bucketedSeries[ti], s)
		}
	}

	// For each group-by time bucket, convert its series into RiakTSQueries:
	riakTSBuckets := make(map[TimeInterval][]RiakTSQuery, len(bucketedSeries))
	for ti, seriesSlice := range bucketedSeries {
		riakTSQueries := make([]RiakTSQuery, len(seriesSlice))
		for i, ser := range seriesSlice {
			start := ti.Start
			end := ti.End

			// the following two special cases ensure equivalency with rounded time boundaries as seen in influxdb:
			// https://docs.influxdata.com/influxdb/v0.13/query_language/data_exploration/#rounded-group-by-time-boundaries
			if start.Before(q.TimeStart) {
				start = q.TimeStart
			}
			if end.After(q.TimeEnd) {
				end = q.TimeEnd
			}

			riakTSQueries[i] = NewRiakTSQuery(string(q.AggregationType), ser.Table, ser.Id, start.UnixNano(), end.UnixNano())
		}
		riakTSBuckets[ti] = riakTSQueries
	}

	qp, err = NewQueryPlanWithServerAggregation(string(q.AggregationType), riakTSBuckets)
	return
}

// ToQueryPlanWithoutServerAggregation combines an HLQuery with a
// ClientSideIndex to make a QueryPlanWithoutServerAggregation.
//
// It executes at most one RiakTSQuery per series.
func (q *HLQuery) ToQueryPlanWithoutServerAggregation(csi *ClientSideIndex) (qp *QueryPlanWithoutServerAggregation, err error) {
	hlQueryInterval := NewTimeInterval(q.TimeStart, q.TimeEnd)
	seriesChoices := csi.SeriesForMeasurementAndField(string(q.MeasurementName), string(q.FieldName))

	// Build the time buckets used for 'group by time'-type queries.
	//
	// It is important to populate these even if they end up being empty,
	// so that we get correct results for empty 'time buckets'.
	timeBuckets := bucketTimeIntervals(q.TimeStart, q.TimeEnd, q.GroupByDuration)

	// For each known db series, use it for querying only if it matches
	// this HLQuery:
	applicableSeries := []Series{}
	for _, s := range seriesChoices {
		if !s.MatchesMeasurementName(string(q.MeasurementName)) {
			continue
		}
		if !s.MatchesFieldName(string(q.FieldName)) {
			continue
		}
		if !s.MatchesTagSets(q.TagSets) {
			continue
		}
		if !s.MatchesTimeInterval(&hlQueryInterval) {
			continue
		}

		applicableSeries = append(applicableSeries, s)
	}

	// Build RiakTSQuery objects that will be used to fulfill this HLQuery:
	riakTSQueries := []RiakTSQuery{}
	for _, ser := range applicableSeries {
		q := NewRiakTSQuery("", ser.Table, ser.Id, q.TimeStart.UnixNano(), q.TimeEnd.UnixNano())
		riakTSQueries = append(riakTSQueries, q)
	}

	qp, err = NewQueryPlanWithoutServerAggregation(string(q.AggregationType), q.GroupByDuration, timeBuckets, riakTSQueries)
	return
}

type RiakTSQuery struct {
	QueryString string
}

// NewRiakTSQuery builds a RiakTSQuery
func NewRiakTSQuery(aggrLabel, tableName, rowName string, timeStartNanos, timeEndNanos int64) RiakTSQuery {
	var queryString string

	if aggrLabel == "" {
		queryString = fmt.Sprintf("SELECT time, value FROM usertable WHERE series = '%s' AND time >= %d AND time < %d", rowName, timeStartNanos, timeEndNanos)
	} else {
		queryString = fmt.Sprintf("SELECT %s(value) FROM usertable WHERE series = '%s' AND time >= %d AND time < %d", aggrLabel, rowName, timeStartNanos, timeEndNanos)
	}
	return RiakTSQuery{queryString}
}

// Type RiakTSResult holds a result from a set of RiakTS aggregation queries.
// Used for debug printing.
type RiakTSResult struct {
	TimeInterval
	Value float64
}