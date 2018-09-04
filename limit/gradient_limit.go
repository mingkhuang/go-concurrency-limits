package limit

import (
	"fmt"
	"github.com/platinummonkey/go-concurrency-limits/core"
	"github.com/platinummonkey/go-concurrency-limits/limit/functions"
	"github.com/platinummonkey/go-concurrency-limits/measurements"
	"math"
	"math/rand"
	"sync"
)

// Gradient limit implements a concurrency limit algorithm that adjust the limits based on the gradient of change in the
// samples minimum RTT and absolute minimum RTT allowing for a queue of square root of the current limit.
// Why square root?  Because it's better than a fixed queue size that becomes too small for large limits but still
// prevents the limit from growing too much by slowing down growth as the limit grows.
type GradientLimit struct {
	estimatedLimit float64 // Estimated concurrency limit based on our algorithm
	maxLimit int // Maximum allowed limit providing an upper bound failsafe
	minLimit int
	queueSizeFunc func(estimatedLimit int) int
	smoothing float64
	rttTolerance float64
	minRTTSampleListener core.MetricSampleListener
	minWindowRTTSampleListener core.MetricSampleListener
	queueSizeSampleListener core.MetricSampleListener
	probeInterval int
	resetRTTCounter int
	rttNoLoadMeasurement core.MeasurementInterface
	logger Logger
	registry core.MetricRegistry

	mu sync.RWMutex
}

func nextProbeCountdown(probeInterval int) int {
	if probeInterval == LimitProbeDisabled {
		return LimitProbeDisabled
	}
	return probeInterval + rand.Int()
}

// NewGradientLimitWithRegistry will create a new GradientLimitWithRegistry.
func NewGradientLimitWithRegistry(
	initialLimit int, // Initial limit used by the limiter
	minLimit int, // Minimum concurrency limit allowed.  The minimum helps prevent the algorithm from adjust the limit too far down.  Note that this limit is not desirable when use as backpressure for batch apps.
	maxConcurrency int,  // Maximum allowable concurrency.  Any estimated concurrency will be capped.
	smoothing float64, // Smoothing factor to limit how aggressively the estimated limit can shrink when queuing has been detected. A smoothing value of 0.0 to 1.0 where 1.0 means the limit is completely replicated by the new estimate.
	queueSizeFunc func(estimatedLimit int) int, // Function to dynamically determine the amount the estimated limit can grow while latencies remain low as a function of the current limit.
	rttTolerance float64, // Tolerance for changes in minimum latency.  Indicating how much change in minimum latency is acceptable before reducing the limit.  For example, a value of 2.0 means that a 2x increase in latency is acceptable.
	probeInterval int, // The limiter will probe for a new noload RTT every probeInterval updates.  Default value is 1000. Set to -1 to disable
	logger Logger, // logger for more information
	registry core.MetricRegistry,
) *GradientLimit {
	if initialLimit <= 0 {
		initialLimit = 50
	}
	if minLimit < 1 {
		minLimit = 1
	}
	if maxConcurrency <= 0 {
		maxConcurrency = 1000
	}
	if smoothing < 0.0 || smoothing > 1.0 {
		smoothing = 0.2
	}
	if rttTolerance < 0 {
		rttTolerance = 2.0
	}
	if probeInterval == 0 {
		probeInterval = 1000
	}
	if queueSizeFunc == nil {
		queueSizeFunc = functions.SqrtRootFunction(4)
	}
	if logger == nil {
		logger = NoopLimitLogger{}
	}

	return &GradientLimit{
		estimatedLimit: float64(initialLimit),
		maxLimit: maxConcurrency,
		minLimit: minLimit,
		queueSizeFunc: queueSizeFunc,
		smoothing: smoothing,
		rttTolerance: rttTolerance,
		probeInterval: probeInterval,
		resetRTTCounter: nextProbeCountdown(probeInterval),
		rttNoLoadMeasurement: &measurements.MaximumMeasurement{},
		logger: logger,
		registry: registry,

		minRTTSampleListener: registry.RegisterDistribution(core.METRIC_MIN_RTT),
		minWindowRTTSampleListener: registry.RegisterDistribution(core.METRIC_WINDOW_MIN_RTT),
		queueSizeSampleListener: registry.RegisterDistribution(core.METRIC_WINDOW_QUEUE_SIZE),
	}
}

func (l *GradientLimit) EstimatedLimit() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return int(l.estimatedLimit)
}

func (l *GradientLimit) RTTNoLoad() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return int64(l.rttNoLoadMeasurement.Get())
}

func (l *GradientLimit) Update(sample core.SampleWindow) {
	if sample.CandidateRTTNanoseconds() <= 0 {
		panic(fmt.Sprintf("rtt must be >0 but got %d", sample.CandidateRTTNanoseconds()))
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	rtt := sample.AverageRTTNanoseconds()
	l.minWindowRTTSampleListener.AddSample(float64(rtt))

	queueSize := l.queueSizeFunc(int(l.estimatedLimit))
	l.queueSizeSampleListener.AddSample(float64(queueSize))

	// Reset or probe for a new noload RTT and a new estimatedLimit.  It's necessary to cut the limit
	// in half to avoid having the limit drift upwards when the RTT is probed during heavy load.
	// To avoid decreasing the limit too much we don't allow it to go lower than the queueSize.
	if l.probeInterval != LimitProbeDisabled {
		l.resetRTTCounter--
		if l.resetRTTCounter <= 0 {
			l.resetRTTCounter = nextProbeCountdown(l.probeInterval)

			l.estimatedLimit = math.Max(float64(l.minLimit), float64(queueSize))
			l.rttNoLoadMeasurement.Reset()
			l.logger.Debugf("probe minRTT limit=%d", int(l.estimatedLimit))
			return
		}
	}

	rttNoLoadFloat, _ := l.rttNoLoadMeasurement.Add(float64(rtt))
	rttNoLoad := int64(rttNoLoadFloat)
	l.minRTTSampleListener.AddSample(float64(rttNoLoad)) // yes we purposely convert back and lose precision

	// Rtt could be higher than rtt_noload because of smoothing rtt noload updates
	// so set to 1.0 to indicate no queuing.  Otherwise calculate the slope and don't
	// allow it to be reduced by more than half to avoid aggressive load-sheding due to
	// outliers.
	gradient := math.Max(0.5, math.Min(1.0, l.rttTolerance * float64(rttNoLoad) / float64(rtt)))

	var newLimit float64
	// Reduce the limit aggressively if there was a drop
	if sample.DidDrop() {
		newLimit = l.estimatedLimit/2
	} else if float64(sample.MaxInFlight()) < l.estimatedLimit / 2 {
		// Don't grow the limit if we are app limited
		return
	} else {
		// Normal update to the limit
		newLimit = l.estimatedLimit * gradient * float64(queueSize)
	}

	if newLimit < l.estimatedLimit {
		// apply downward smoothing with a minLimit minimum.
		newLimit = math.Max(float64(l.minLimit), l.estimatedLimit * (1-l.smoothing) + l.smoothing*newLimit)
	}
	newLimit = math.Max(float64(queueSize), math.Min(float64(l.maxLimit), newLimit))

	if int(newLimit) != int(l.estimatedLimit) && l.logger.IsDebugEnabled() {
		l.logger.Debugf("new limit=%d, minRtt=%d ms, winRtt=%d ms, queueSize=%d, gradient=%0.4f, resetCounter=%d",
			int(newLimit), rttNoLoad / 1e6, rtt / 1e6, queueSize, gradient, l.resetRTTCounter)
	}

	l.estimatedLimit = newLimit
}

func (l GradientLimit) String() string {
	return fmt.Sprintf("GradientLimit{limit=%d, rttNoLoad=%d ms}",
		l.EstimatedLimit(), l.RTTNoLoad() / 1e6)
}

