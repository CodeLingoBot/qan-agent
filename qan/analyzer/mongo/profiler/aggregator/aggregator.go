package aggregator

import (
	"strings"
	"sync"
	"time"

	"github.com/percona/go-mysql/event"
	"github.com/percona/percona-toolkit/src/go/mongolib/fingerprinter"
	"github.com/percona/percona-toolkit/src/go/mongolib/proto"
	"github.com/percona/percona-toolkit/src/go/mongolib/stats"
	pc "github.com/percona/pmm/proto/config"
	"github.com/percona/pmm/proto/qan"
	"github.com/percona/qan-agent/qan/analyzer/report"
)

const (
	DefaultInterval       = 60 // in seconds
	DefaultExampleQueries = true
)

// New returns configured *Aggregator
func New(timeStart time.Time, config pc.QAN) *Aggregator {
	// verify config
	if config.Interval == 0 {
		config.Interval = DefaultInterval
		config.ExampleQueries = DefaultExampleQueries
	}

	aggregator := &Aggregator{
		config: config,
	}

	// create duration from interval
	aggregator.D = time.Duration(config.Interval) * time.Second

	// create mongolib stats
	fp := fingerprinter.NewFingerprinter(fingerprinter.DEFAULT_KEY_FILTERS)
	aggregator.stats = stats.New(fp)

	// create new interval
	aggregator.newInterval(timeStart)

	return aggregator
}

// Aggregator aggregates system.profile document
type Aggregator struct {
	// dependencies
	config pc.QAN

	// interval
	timeStart time.Time
	timeEnd   time.Time
	D         time.Duration
	stats     *stats.Stats

	// make it safe to use from different threads
	sync.Mutex
}

// Add aggregates new system.profile document and returns report if it's ready
func (self *Aggregator) Add(doc proto.SystemProfile) (*qan.Report, error) {
	self.Lock()
	defer self.Unlock()

	ts := doc.Ts.UTC()

	// skip old metrics
	if ts.Before(self.timeStart) {
		return nil, nil
	}

	return self.interval(ts), self.stats.Add(doc)
}

// Report generates report for current interval and starts new one
func (self *Aggregator) Report() *qan.Report {
	self.Lock()
	defer self.Unlock()

	return self.interval(time.Now())
}

// interval sets interval if necessary and returns *qan.Report for old interval if not empty
func (self *Aggregator) interval(ts time.Time) *qan.Report {
	// if time is before interval end then we are still in the same interval, nothing to do
	if ts.Before(self.timeEnd) {
		return nil
	}

	// create new interval
	defer self.newInterval(ts)

	// let's check if we have anything to send for current interval
	if len(self.stats.Queries()) == 0 {
		// if there are no queries then we don't create report #PMM-927
		return nil
	}

	// create result
	result := self.createResult()

	// translate result into report and return it
	return report.MakeReport(self.config, self.timeStart, self.timeEnd, nil, result)
}

// TimeStart returns start time for current interval
func (self *Aggregator) TimeStart() time.Time {
	return self.timeStart
}

// TimeEnd returns end time for current interval
func (self *Aggregator) TimeEnd() time.Time {
	return self.timeEnd
}

func (self *Aggregator) newInterval(ts time.Time) {
	// reset stats
	self.stats.Reset()

	// truncate to the duration e.g 12:15:35 with 1 minute duration it will be 12:15:00
	self.timeStart = ts.UTC().Truncate(self.D)
	// create ending time by adding interval
	self.timeEnd = self.timeStart.Add(self.D)
}

func (self *Aggregator) createResult() *report.Result {
	queries := self.stats.Queries()
	global := event.NewClass("", "", false)
	queryStats := queries.CalcQueriesStats(int64(self.config.Interval))
	classes := []*event.Class{}
	for _, queryInfo := range queryStats {
		class := event.NewClass(queryInfo.ID, queryInfo.Fingerprint, self.config.ExampleQueries)
		if self.config.ExampleQueries {
			db := ""
			s := strings.SplitN(queryInfo.Namespace, ".", 2)
			if len(s) == 2 {
				db = s[0]
			}

			class.Example = &event.Example{
				QueryTime: queryInfo.QueryTime.Total,
				Db:        db,
				Query:     queryInfo.Query,
			}
		}

		metrics := event.NewMetrics()

		metrics.TimeMetrics["Query_time"] = newEventTimeStatsInMilliseconds(queryInfo.QueryTime)

		// @todo we map below metrics to MySQL equivalents according to PMM-830
		metrics.NumberMetrics["Bytes_sent"] = newEventNumberStats(queryInfo.ResponseLength)
		metrics.NumberMetrics["Rows_sent"] = newEventNumberStats(queryInfo.Returned)
		metrics.NumberMetrics["Rows_examined"] = newEventNumberStats(queryInfo.Scanned)

		class.Metrics = metrics
		class.TotalQueries = uint(queryInfo.Count)
		class.UniqueQueries = 1
		classes = append(classes, class)

		// Add the class to the global metrics.
		global.AddClass(class)
	}

	return &report.Result{
		Global: global,
		Class:  classes,
	}

}

func newEventNumberStats(s stats.Statistics) *event.NumberStats {
	return &event.NumberStats{
		Sum: uint64(s.Total),
		Min: uint64(s.Min),
		Avg: uint64(s.Avg),
		Med: uint64(s.Median),
		P95: uint64(s.Pct95),
		Max: uint64(s.Max),
	}
}

func newEventTimeStatsInMilliseconds(s stats.Statistics) *event.TimeStats {
	return &event.TimeStats{
		Sum: s.Total / 1000,
		Min: s.Min / 1000,
		Avg: s.Avg / 1000,
		Med: s.Median / 1000,
		P95: s.Pct95 / 1000,
		Max: s.Max / 1000,
	}
}
