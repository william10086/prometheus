// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package promql

import (
	"fmt"
	"io/ioutil"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/local"
	"github.com/prometheus/prometheus/storage/metric"
	"github.com/prometheus/prometheus/utility"
	testutil "github.com/prometheus/prometheus/utility/test"
)

var (
	minNormal = math.Float64frombits(0x0010000000000000) // The smallest positive normal value of type float64.
	spacePat  = regexp.MustCompile("[\t\r ]+")
)

const (
	testStartTime = clientmodel.Timestamp(0)
	epsilon       = 0.000001 // Relative error allowed for sample values.
	maxErrorCount = 10
)

// Test is a sequence of read and write commands that are run
// against a test storage.
type Test struct {
	*testing.T

	cmds []testCommand

	storage      local.Storage
	closeStorage func()
	queryEngine  *Engine
}

// NewTest returns an initialized empty Test.
func NewTest(t *testing.T, input string) (*Test, error) {
	test := &Test{
		T:    t,
		cmds: make([]testCommand, 0),
	}
	err := test.parse(input)
	test.clear()

	return test, err
}

func NewTestFromFile(t *testing.T, filename string) (*Test, error) {
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return NewTest(t, string(content))
}

// parse the given command sequence and appends it to the test.
func (t *Test) parse(input string) error {
	// Trim lines and remove comments.
	lines := strings.Split(input, "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") {
			l = ""
		}
		lines[i] = l
	}

	// Scan for steps line by line.
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if len(l) == 0 {
			continue
		}
		var cmd testCommand

		switch p := spacePat.Split(l, 4); p[0] {
		case "clear":
			cmd = &clearCmd{}

		case "load":
			if len(p) < 2 {
				return &ParseErr{
					Line: i + 1,
					Err:  fmt.Errorf("missing arguments to define command. (load <step>)"),
				}
			}
			gap, err := utility.StringToDuration(p[1])
			if err != nil {
				return &ParseErr{
					Line: i + 1,
					Err:  fmt.Errorf("invalid step definition: %s", err),
				}
			}
			def := newLoadCmd(gap)
			for j := 0; i+1 < len(lines); j++ {
				i++
				defLine := lines[i]
				if len(defLine) == 0 {
					i--
					break
				}
				metric, vals, err := parseSeries(defLine)
				if err != nil {
					perr := err.(*ParseErr)
					perr.Line = i + 1
					return err
				}
				def.set(metric, vals...)
			}
			cmd = def

		case "eval", "eval_ordered", "eval_fail":
			if len(p) < 4 {
				return &ParseErr{
					Line: i + 1,
					Err:  fmt.Errorf("missing arguments to eval command. (eval instant|range <time bounds> <query>"),
				}
			}
			expr, err := ParseExpr(p[3])
			if err != nil {
				perr := err.(*ParseErr)
				perr.Line = i + 1
				perr.Pos += strings.Index(l, p[3])
				return perr
			}

			var (
				start, end clientmodel.Timestamp
				interval   time.Duration
			)

			if !strings.Contains(p[2], ":") {
				if p[1] != "instant" {
					return &ParseErr{
						Line: i + 1,
						Err:  fmt.Errorf("invalid duration for instant evaluation"),
					}
				}
				offset, err := utility.StringToDuration(p[2])
				if err != nil {
					return &ParseErr{
						Line: i + 1,
						Err:  err,
					}
				}
				start = testStartTime.Add(offset)
				end = start

			} else if timeBounds := strings.Split(p[2], ":"); len(timeBounds) == 3 {
				if p[1] != "range" {
					return &ParseErr{
						Line: i + 1,
						Err:  fmt.Errorf("invalid duration for range evaluation"),
					}
				}
				offset, err := utility.StringToDuration(timeBounds[0])
				if err != nil {
					return &ParseErr{
						Line: i + 1,
						Err:  err,
					}
				}
				start = testStartTime.Add(offset)

				offset, err = utility.StringToDuration(timeBounds[1])
				if err != nil {
					return &ParseErr{
						Line: i + 1,
						Err:  err,
					}
				}
				end = testStartTime.Add(offset)

				interval, err = utility.StringToDuration(timeBounds[2])
				if err != nil {
					return &ParseErr{
						Line: i + 1,
						Err:  err,
					}
				}
			} else {
				return &ParseErr{
					Line: i + 1,
					Err:  fmt.Errorf("invalid time bounds"),
				}
			}
			evs := newEvalCmd(expr, start, end, interval)
			if strings.HasSuffix(p[0], "ordered") {
				evs.ordered = true
			}
			if strings.HasSuffix(p[0], "fail") {
				evs.fail = true
			}

			for j := 1; i+1 < len(lines); j++ {
				i++
				defLine := lines[i]
				if len(defLine) == 0 {
					i--
					break
				}
				if f, err := parseNumber(defLine); err == nil {
					evs.expect(0, nil, clientmodel.SampleValue(f))
					break
				}
				metric, vals, err := parseSeries(defLine)
				if err != nil {
					perr := err.(*ParseErr)
					perr.Line = i + 1
					return err
				}

				if p[1] != "range" && len(vals) > 1 {
					return &ParseErr{
						Line: i + 1,
						Err:  fmt.Errorf("expecting multiple values in instant evaluation not allowed"),
					}
				}
				evs.expect(j, metric, vals...)
			}
			cmd = evs

		default:
			return &ParseErr{
				Line: i + 1,
				Err:  fmt.Errorf("invalid command %q", l),
			}
		}
		t.cmds = append(t.cmds, cmd)
	}
	return nil
}

// testCommand is an interface that ensures that only the package internal
// types can be a valid command for a test.
type testCommand interface {
	testCmd() string
}

func (*clearCmd) testCmd() string { return "clear" }
func (*loadCmd) testCmd() string  { return "load" }
func (*evalCmd) testCmd() string  { return "eval" }

// loadCmd is a command that loads sequences of sample values for specific
// metrics into the storage.
type loadCmd struct {
	gap     time.Duration
	metrics map[clientmodel.Fingerprint]clientmodel.Metric
	defs    map[clientmodel.Fingerprint]metric.Values
}

func newLoadCmd(gap time.Duration) *loadCmd {
	return &loadCmd{
		gap:     gap,
		metrics: make(map[clientmodel.Fingerprint]clientmodel.Metric),
		defs:    make(map[clientmodel.Fingerprint]metric.Values),
	}
}

// set a sequence of sample values for the given metric.
func (def *loadCmd) set(m clientmodel.Metric, vals ...clientmodel.SampleValue) {
	fp := m.Fingerprint()

	samples := make(metric.Values, 0, len(vals))
	ts := testStartTime
	for _, v := range vals {
		samples = append(samples, metric.SamplePair{
			Timestamp: ts,
			Value:     v,
		})
		ts = ts.Add(def.gap)
	}

	def.defs[fp] = samples
	def.metrics[fp] = m
}

// append the defined time series to the storage.
func (def *loadCmd) append(a storage.SampleAppender) {
	for fp, samples := range def.defs {
		met := def.metrics[fp]
		for _, smpl := range samples {
			s := &clientmodel.Sample{
				Metric:    met,
				Value:     smpl.Value,
				Timestamp: smpl.Timestamp,
			}
			a.Append(s)
		}
	}
}

// evalCmd is a command that evaluates an expression for the given time (range)
// and expects a specific result.
type evalCmd struct {
	expr       Expr
	start, end clientmodel.Timestamp
	interval   time.Duration

	instant       bool
	fail, ordered bool

	metrics  map[clientmodel.Fingerprint]clientmodel.Metric
	expected map[clientmodel.Fingerprint]entry
}

type entry struct {
	pos  int
	vals []clientmodel.SampleValue
}

func newEvalCmd(expr Expr, start, end clientmodel.Timestamp, interval time.Duration) *evalCmd {
	return &evalCmd{
		expr:     expr,
		start:    start,
		end:      end,
		interval: interval,
		instant:  start == end && interval == 0,

		metrics:  make(map[clientmodel.Fingerprint]clientmodel.Metric),
		expected: make(map[clientmodel.Fingerprint]entry),
	}
}

// expect adds a new metric with a sequence of values to the set of expected
// results for the query.
func (ev *evalCmd) expect(pos int, m clientmodel.Metric, vals ...clientmodel.SampleValue) {
	if m == nil {
		ev.expected[0] = entry{pos: pos, vals: vals}
		return
	}
	fp := m.Fingerprint()
	ev.metrics[fp] = m
	ev.expected[fp] = entry{pos: pos, vals: vals}
}

// compareResult compares the result value with the defined expectation.
func (ev *evalCmd) compareResult(result Value) error {
	switch val := result.(type) {
	case Matrix:
		if ev.instant {
			return fmt.Errorf("received range result on instant evaluation")
		}
		seen := make(map[clientmodel.Fingerprint]bool)
		for pos, v := range val {
			fp := v.Metric.Metric.Fingerprint()
			if _, ok := ev.metrics[fp]; !ok {
				return fmt.Errorf("unexpected metric %s in result", v.Metric.Metric)
			}
			exp := ev.expected[fp]
			if ev.ordered && exp.pos != pos+1 {
				return fmt.Errorf("expected metric %s with %v at position %d but was at %d", v.Metric.Metric, exp.vals, exp.pos, pos+1)
			}
			for i, expVal := range exp.vals {
				if !almostEqual(float64(expVal), float64(v.Values[i].Value)) {
					return fmt.Errorf("expected %v for %s but got %v", expVal, v.Metric.Metric, v.Values)
				}
			}
			seen[fp] = true
		}
		for fp, expVals := range ev.expected {
			if !seen[fp] {
				return fmt.Errorf("expected metric %s with %v not found", ev.metrics[fp], expVals)
			}
		}

	case Vector:
		if !ev.instant {
			fmt.Errorf("received instant result on range evaluation")
		}
		seen := make(map[clientmodel.Fingerprint]bool)
		for pos, v := range val {
			fp := v.Metric.Metric.Fingerprint()
			if _, ok := ev.metrics[fp]; !ok {
				return fmt.Errorf("metric %s not in expected", v.Metric.Metric)
			}
			exp := ev.expected[fp]
			if ev.ordered && exp.pos != pos+1 {
				return fmt.Errorf("expected metric %s with %v at position %d but was at %d", v.Metric.Metric, exp.vals, exp.pos, pos+1)
			}
			if !almostEqual(float64(exp.vals[0]), float64(v.Value)) {
				return fmt.Errorf("expected %v for %s but got %v", exp.vals[0], v.Metric.Metric, v.Value)
			}

			seen[fp] = true
		}
		for fp, expVals := range ev.expected {
			if !seen[fp] {
				return fmt.Errorf("expected metric %s with %v not found", ev.metrics[fp], expVals)
			}
		}

	case *Scalar:
		if !almostEqual(float64(ev.expected[0].vals[0]), float64(val.Value)) {
			return fmt.Errorf("expected scalar %v but got %v", val.Value, ev.expected[0].vals[0])
		}

	default:
		panic(fmt.Errorf("promql.Test.compareResult: unexpected result type %T", result))
	}
	return nil
}

// clearCmd is a command that wipes the test's storage state.
type clearCmd struct{}

// Run executes the command sequence of the test. Until the maximum error number
// is reached, evaluation errors do not terminate execution.
func (t *Test) Run() error {
	for _, cmd := range t.cmds {
		err := t.exec(cmd)
		// TODO(fabxc): aggregate command errors, yield diffs for result
		// comparison errors.
		if err != nil {
			return err
		}
	}
	return nil
}

// exec processes a single step of the test
func (t *Test) exec(tc testCommand) error {
	switch cmd := tc.(type) {
	case *clearCmd:
		t.clear()

	case *loadCmd:
		cmd.append(t.storage)
		t.storage.WaitForIndexing()

	case *evalCmd:
		q := t.queryEngine.newQuery(cmd.expr, cmd.start, cmd.end, cmd.interval)
		res := q.Exec()
		if res.Err != nil {
			if cmd.fail {
				return nil
			}
			return fmt.Errorf("error evaluating query: %s", res.Err)
		}
		if res.Err == nil && cmd.fail {
			return fmt.Errorf("expected error evaluating query but got none")
		}

		err := cmd.compareResult(res.Value)
		if err != nil {
			return fmt.Errorf("error in %s %s: %s", cmd.testCmd(), cmd.expr, err)
		}

	default:
		panic("promql.Test.exec: unknown test command type")
	}
	return nil
}

// clear the current test storage of all inserted samples.
func (t *Test) clear() {
	if t.closeStorage != nil {
		t.closeStorage()
	}
	if t.queryEngine != nil {
		t.queryEngine.Stop()
	}

	var closer testutil.Closer
	t.storage, closer = local.NewTestStorage(t, 1)

	t.closeStorage = closer.Close
	t.queryEngine = NewEngine(t.storage)
}

func (t *Test) Close() {
	t.queryEngine.Stop()
	t.closeStorage()
}

// samplesAlmostEqual returns true if the two sample lines only differ by a
// small relative error in their sample value.
func almostEqual(a, b float64) bool {
	// NaN has no equality but for testing we still want to know whether both values
	// are NaN.
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}

	// Cf. http://floating-point-gui.de/errors/comparison/
	if a == b {
		return true
	}

	diff := math.Abs(a - b)

	if a == 0 || b == 0 || diff < minNormal {
		return diff < epsilon*minNormal
	}
	return diff/(math.Abs(a)+math.Abs(b)) < epsilon
}

func parseNumber(s string) (float64, error) {
	n, err := strconv.ParseInt(s, 0, 64)
	f := float64(n)
	if err != nil {
		f, err = strconv.ParseFloat(s, 64)
	}
	if err != nil {
		return 0, fmt.Errorf("error parsing number: %s", err)
	}
	return f, nil
}
