// Copyright 2019, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package signalfxexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/signalfxexporter"

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/signalfxexporter/internal/dimensions"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/signalfxexporter/internal/hostmetadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/signalfxexporter/internal/translation"
	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/splunk"
	metadata "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/experimentalmetricmetadata"
)

// TODO: Find a place for this to be shared.
type baseMetricsExporter struct {
	component.Component
	consumer.Metrics
}

// TODO: Find a place for this to be shared.
type baseLogsExporter struct {
	component.Component
	consumer.Logs
}

type signalfMetadataExporter struct {
	exporter.Metrics
	pushMetadata func(metadata []*metadata.MetadataUpdate) error
}

func (sme *signalfMetadataExporter) ConsumeMetadata(metadata []*metadata.MetadataUpdate) error {
	return sme.pushMetadata(metadata)
}

type signalfxExporter struct {
	config             *Config
	logger             *zap.Logger
	telemetrySettings  component.TelemetrySettings
	pushMetricsData    func(ctx context.Context, md pmetric.Metrics) (droppedTimeSeries int, err error)
	pushMetadata       func(metadata []*metadata.MetadataUpdate) error
	pushLogsData       func(ctx context.Context, ld plog.Logs) (droppedLogRecords int, err error)
	hostMetadataSyncer *hostmetadata.Syncer
	converter          *translation.MetricsConverter
}

type exporterOptions struct {
	ingestURL         *url.URL
	ingestTLSSettings configtls.TLSClientSetting
	apiURL            *url.URL
	apiTLSSettings    configtls.TLSClientSetting
	httpTimeout       time.Duration
	token             string
	logDataPoints     bool
	logDimUpdate      bool
	metricTranslator  *translation.MetricTranslator
}

// newSignalFxExporter returns a new SignalFx exporter.
func newSignalFxExporter(
	config *Config,
	createSettings exporter.CreateSettings,
) (*signalfxExporter, error) {
	if config == nil {
		return nil, errors.New("nil config")
	}

	options, err := config.getOptionsFromConfig()
	if err != nil {
		return nil, err
	}

	sampledLogger := translation.CreateSampledLogger(createSettings.Logger)
	converter, err := translation.NewMetricsConverter(
		sampledLogger,
		options.metricTranslator,
		config.ExcludeMetrics,
		config.IncludeMetrics,
		config.NonAlphanumericDimensionChars,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create metric converter: %w", err)
	}

	return &signalfxExporter{
		config:            config,
		logger:            createSettings.Logger,
		telemetrySettings: createSettings.TelemetrySettings,
		converter:         converter,
	}, nil
}

func (se *signalfxExporter) start(_ context.Context, host component.Host) (err error) {
	options, err := se.config.getOptionsFromConfig()
	if err != nil {
		return err
	}

	headers := buildHeaders(se.config)
	client, err := se.createClient(host)
	if err != nil {
		return err
	}

	dpClient := &sfxDPClient{
		sfxClientBase: sfxClientBase{
			ingestURL: options.ingestURL,
			headers:   headers,
			client:    client,
			zippers:   newGzipPool(),
		},
		logDataPoints:          options.logDataPoints,
		logger:                 se.logger,
		accessTokenPassthrough: se.config.AccessTokenPassthrough,
		converter:              se.converter,
	}

	apiTLSCfg, err := se.config.APITLSSettings.LoadTLSConfig()
	if err != nil {
		return fmt.Errorf("could not load API TLS config: %w", err)
	}

	dimClient := dimensions.NewDimensionClient(
		context.Background(),
		dimensions.DimensionClientOptions{
			Token:        options.token,
			APIURL:       options.apiURL,
			APITLSConfig: apiTLSCfg,
			LogUpdates:   options.logDimUpdate,
			Logger:       se.logger,
			// Duration to wait between property updates. This might be worth
			// being made configurable.
			SendDelay: 10,
			// In case of having issues sending dimension updates to SignalFx,
			// buffer a fixed number of updates. Might also be a good candidate
			// to make configurable.
			PropertiesMaxBuffered: 10000,
			MetricsConverter:      *se.converter,
		})
	dimClient.Start()

	var hms *hostmetadata.Syncer
	if se.config.SyncHostMetadata {
		hms = hostmetadata.NewSyncer(se.logger, dimClient)
	}
	se.pushMetricsData = dpClient.pushMetricsData
	se.pushMetadata = dimClient.PushMetadata
	se.hostMetadataSyncer = hms
	return nil
}

func newGzipPool() sync.Pool {
	return sync.Pool{New: func() interface{} {
		return gzip.NewWriter(nil)
	}}
}

func newEventExporter(config *Config, createSettings exporter.CreateSettings) (*signalfxExporter, error) {
	if config == nil {
		return nil, errors.New("nil config")
	}

	_, err := config.getOptionsFromConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to process config: %w", err)
	}
	return &signalfxExporter{
		config:            config,
		logger:            createSettings.Logger,
		telemetrySettings: createSettings.TelemetrySettings,
	}, nil

}

func (se *signalfxExporter) startLogs(_ context.Context, host component.Host) error {
	options, err := se.config.getOptionsFromConfig()
	if err != nil {
		return fmt.Errorf("failed to process config: %w", err)
	}

	headers := buildHeaders(se.config)
	client, err := se.createClient(host)
	if err != nil {
		return err
	}

	eventClient := &sfxEventClient{
		sfxClientBase: sfxClientBase{
			ingestURL: options.ingestURL,
			headers:   headers,
			client:    client,
			zippers:   newGzipPool(),
		},
		logger:                 se.logger,
		accessTokenPassthrough: se.config.AccessTokenPassthrough,
	}

	se.pushLogsData = eventClient.pushLogsData
	return nil
}

func (se *signalfxExporter) createClient(host component.Host) (*http.Client, error) {
	se.config.HTTPClientSettings.TLSSetting = se.config.IngestTLSSettings

	if se.config.HTTPClientSettings.MaxIdleConns == nil {
		se.config.HTTPClientSettings.MaxIdleConns = &se.config.MaxConnections
	}
	if se.config.HTTPClientSettings.MaxIdleConnsPerHost == nil {
		se.config.HTTPClientSettings.MaxIdleConnsPerHost = &se.config.MaxConnections
	}
	if se.config.HTTPClientSettings.IdleConnTimeout == nil {
		defaultIdleConnTimeout := 30 * time.Second
		se.config.HTTPClientSettings.IdleConnTimeout = &defaultIdleConnTimeout
	}

	return se.config.ToClient(host, se.telemetrySettings)
}

func (se *signalfxExporter) pushMetrics(ctx context.Context, md pmetric.Metrics) error {
	_, err := se.pushMetricsData(ctx, md)
	if err == nil && se.hostMetadataSyncer != nil {
		se.hostMetadataSyncer.Sync(md)
	}
	return err
}

func (se *signalfxExporter) pushLogs(ctx context.Context, ld plog.Logs) error {
	_, err := se.pushLogsData(ctx, ld)
	return err
}

func buildHeaders(config *Config) map[string]string {
	headers := map[string]string{
		"Connection":   "keep-alive",
		"Content-Type": "application/x-protobuf",
		"User-Agent":   "OpenTelemetry-Collector SignalFx Exporter/v0.0.1",
	}

	if config.AccessToken != "" {
		headers[splunk.SFxAccessTokenHeader] = config.AccessToken
	}

	// Add any custom headers from the config. They will override the pre-defined
	// ones above in case of conflict, but, not the content encoding one since
	// the latter one is defined according to the payload.
	for k, v := range config.HTTPClientSettings.Headers {
		headers[k] = string(v)
	}
	// we want to control how headers are set, overriding user headers with our passthrough.
	config.HTTPClientSettings.Headers = nil

	return headers
}
