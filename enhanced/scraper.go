package enhanced

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"

	"github.com/percona/rds_exporter/sessions"
)

// scraper retrieves metrics from several RDS instances sharing a single session.
type scraper struct {
	instances      []sessions.Instance
	logStreamNames []string
	svc            *cloudwatchlogs.CloudWatchLogs
	nextStartTime  time.Time
	logger         log.Logger
}

func newScraper(session *session.Session, instances []sessions.Instance) *scraper {
	logStreamNames := make([]string, 0, len(instances))
	for _, instance := range instances {
		logStreamNames = append(logStreamNames, instance.ResourceID)
	}

	return &scraper{
		instances:      instances,
		logStreamNames: logStreamNames,
		svc:            cloudwatchlogs.New(session),
		nextStartTime:  time.Now().Add(-3 * time.Minute).Round(0), // strip monotonic clock reading
		logger:         log.With("component", "enhanced"),
	}
}

// start scrapes metrics in loop and sends them to the channel until context is canceled.
func (s *scraper) start(ctx context.Context, ch chan<- map[string][]prometheus.Metric) {
	interval := time.Minute
	for _, instance := range s.instances {
		if instance.EnhancedMonitoringInterval > 0 && instance.EnhancedMonitoringInterval < interval {
			interval = instance.EnhancedMonitoringInterval
		}
	}
	s.logger.Infof("Updating enhanced metrics every %s.", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// nothing
		case <-ctx.Done():
			return
		}

		ch <- s.scrape(ctx)
	}
}

// scrape performs a single scrape.
func (s *scraper) scrape(ctx context.Context) map[string][]prometheus.Metric {
	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName:   aws.String("RDSOSMetrics"),
		LogStreamNames: aws.StringSlice(s.logStreamNames),
		StartTime:      aws.Int64(aws.TimeUnixMilli(s.nextStartTime)),
	}
	s.logger.Debugf("Requesting metrics since %s (last %s).", s.nextStartTime, time.Since(s.nextStartTime))

	// collect all returned events and metrics
	allMetrics := make(map[string]map[time.Time][]prometheus.Metric) // ResourceID -> event timestamp -> metrics
	collectAllMetrics := func(output *cloudwatchlogs.FilterLogEventsOutput, lastPage bool) bool {
		for _, event := range output.Events {
			l := s.logger.With("EventId", *event.EventId).With("LogStreamName", *event.LogStreamName)
			l = l.With("Timestamp", aws.MillisecondsTimeValue(event.Timestamp).UTC())
			l = l.With("IngestionTime", aws.MillisecondsTimeValue(event.IngestionTime).UTC())

			var instance *sessions.Instance
			for _, i := range s.instances {
				if i.ResourceID == *event.LogStreamName {
					instance = &i
					break
				}
			}
			if instance == nil {
				l.Errorf("Failed to find instance.")
				continue
			}
			l = l.With("region", instance.Region).With("instance", instance.Instance)

			// l.Debugf("Message:\n%s", *event.Message)
			osMetrics, err := parseOSMetrics([]byte(*event.Message))
			if err != nil {
				l.Errorf("Failed to parse metrics: %s.", err)
				continue
			}
			// l.Debugf("OS Metrics:\n%#v", osMetrics)

			if allMetrics[instance.ResourceID] == nil {
				allMetrics[instance.ResourceID] = make(map[time.Time][]prometheus.Metric)
			}
			timestamp := aws.MillisecondsTimeValue(event.Timestamp)
			metrics := osMetrics.makePrometheusMetrics(instance.Region)
			allMetrics[instance.ResourceID][timestamp] = metrics
			l.Debugf("Timestamp from Message: %s.", osMetrics.Timestamp.UTC())
		}

		return true // continue pagination
	}
	if err := s.svc.FilterLogEventsPagesWithContext(ctx, input, collectAllMetrics); err != nil {
		s.logger.Errorf("Failed to filter log events: %s.", err)
	}

	// get better times
	allTimes := make(map[string][]time.Time)
	for resourceID, events := range allMetrics {
		allTimes[resourceID] = make([]time.Time, 0, len(events))
		for timestamp := range events {
			allTimes[resourceID] = append(allTimes[resourceID], timestamp)
		}
	}
	var times map[string]time.Time
	times, s.nextStartTime = betterTimes(allTimes)

	// return only latest metrics
	res := make(map[string][]prometheus.Metric) // ResourceID -> metrics
	for resourceID, timestamp := range times {
		res[resourceID] = allMetrics[resourceID][timestamp]
	}
	return res
}

// betterTimes returns timestamps of the latest metrics, and also StarTime that should be used in the next request
func betterTimes(allTimes map[string][]time.Time) (times map[string]time.Time, nextStartTime time.Time) {
	// keep only the most recent metrics for each instance
	nextStartTime = time.Now()
	times = make(map[string]time.Time) // ResourceID -> timestamp
	for resourceID, events := range allTimes {
		var newest time.Time
		for _, timestamp := range events {
			if newest.Before(timestamp) {
				newest = timestamp
				times[resourceID] = timestamp
			}
		}

		if nextStartTime.After(newest) {
			nextStartTime = newest
		}
	}

	return
}
