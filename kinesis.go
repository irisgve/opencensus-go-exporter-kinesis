// Copyright 2019, Omnition
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

// Package jaeger contains an OpenCensus tracing exporter for AWS Kinesis.
package kinesis // import "github.com/omnition/opencensus-go-exporter-kinesis"

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
	tracepb "github.com/census-instrumentation/opencensus-proto/gen-go/trace/v1"
	gogoproto "github.com/gogo/protobuf/proto"
	"github.com/golang/protobuf/proto"

	gen "github.com/jaegertracing/jaeger/model"
	producer "github.com/omnition/omnition-kinesis-producer"
	"go.opencensus.io/stats/view"
	"go.uber.org/zap"
)

const (
	encodingJaeger = "jaeger-proto"
	encodingOC     = "oc-proto"
)

var supportedEncodings = [2]string{encodingJaeger, encodingOC}

// Options are the options to be used when initializing a Jaeger exporter.
type Options struct {
	Name                    string
	StreamName              string
	AWSRegion               string
	AWSRole                 string
	AWSKinesisEndpoint      string
	QueueSize               int
	NumWorkers              int
	MaxListSize             int
	ListFlushInterval       int
	KPLAggregateBatchCount  int
	KPLAggregateBatchSize   int
	KPLBatchSize            int
	KPLBatchCount           int
	KPLBacklogCount         int
	KPLFlushIntervalSeconds int
	KPLMaxConnections       int
	KPLMaxRetries           int
	KPLMaxBackoffSeconds    int
	MaxAllowedSizePerSpan   int

	// Encoding defines the format in which spans should be exporter to kinesis
	// only Jaeger is supported right now
	Encoding string
}

func (o Options) isValidEncoding() bool {
	for _, e := range supportedEncodings {
		if e == o.Encoding {
			return true
		}
	}
	return false
}

// NewExporter returns a trace.Exporter implementation that exports
// the collected spans to Jaeger.
func NewExporter(o Options, logger *zap.Logger) (*Exporter, error) {

	if o.MaxListSize == 0 {
		o.MaxListSize = 100000
	}

	if o.ListFlushInterval == 0 {
		o.ListFlushInterval = 5
	}

	if o.MaxAllowedSizePerSpan == 0 {
		o.MaxAllowedSizePerSpan = 900000
	}

	if o.QueueSize == 0 {
		o.QueueSize = 100000
	}
	if o.NumWorkers == 0 {
		o.NumWorkers = 8
	}
	if o.AWSRegion == "" {
		return nil, errors.New("missing AWS Region for Kinesis exporter")
	}

	if o.StreamName == "" {
		return nil, errors.New("missing Stream Name for Kinesis exporter")
	}

	if o.Encoding == "" {
		o.Encoding = encodingJaeger
	}

	if !o.isValidEncoding() {
		return nil, fmt.Errorf("invalid option for Encoding. Valid choices are: %v", supportedEncodings)
	}

	sess := session.Must(session.NewSession(aws.NewConfig().WithRegion(o.AWSRegion)))
	cfgs := []*aws.Config{}
	if o.AWSRole != "" {
		cfgs = append(cfgs, &aws.Config{Credentials: stscreds.NewCredentials(sess, o.AWSRole)})
	}
	if o.AWSKinesisEndpoint != "" {
		cfgs = append(cfgs, &aws.Config{Endpoint: aws.String(o.AWSKinesisEndpoint)})
	}
	client := kinesis.New(sess, cfgs...)

	shards, err := getShards(client, o.StreamName)
	if err != nil {
		return nil, err
	}

	producers := make([]*shardProducer, 0, len(shards))
	for _, shard := range shards {
		hooks := newKinesisHooks(o.Name, o.StreamName, shard.shardId)
		pr := producer.New(&producer.Config{
			StreamName:          o.StreamName,
			AggregateBatchSize:  o.KPLAggregateBatchSize,
			AggregateBatchCount: o.KPLAggregateBatchCount,
			BatchSize:           o.KPLBatchSize,
			BatchCount:          o.KPLBatchCount,
			BacklogCount:        o.KPLBacklogCount,
			MaxConnections:      o.KPLMaxConnections,
			FlushInterval:       time.Second * time.Duration(o.KPLFlushIntervalSeconds),
			MaxRetries:          o.KPLMaxRetries,
			MaxBackoffTime:      time.Second * time.Duration(o.KPLMaxBackoffSeconds),
			Client:              client,
			Verbose:             false,
		}, hooks)
		producers = append(producers, &shardProducer{
			pr:            pr,
			shard:         shard,
			hooks:         hooks,
			maxSize:       uint64(o.MaxListSize),
			flushInterval: time.Duration(o.ListFlushInterval) * time.Second,
			partitionKey:  shard.startingHashKey.String(),
			isJaeger:      o.Encoding == encodingJaeger,
		})
	}

	e := &Exporter{
		options:   &o,
		producers: producers,
		logger:    logger,
		hooks:     newKinesisHooks(o.Name, o.StreamName, ""),
		semaphore: nil,
	}

	maxReceivers, _ := strconv.Atoi(os.Getenv("MAX_KINESIS_RECEIVERS"))
	if maxReceivers > 0 {
		e.semaphore = make(chan struct{}, maxReceivers)
	}

	v := metricViews()
	if err := view.Register(v...); err != nil {
		return nil, err
	}

	for _, sp := range e.producers {
		sp.start()
	}

	return e, nil
}

// Exporter takes spans in jaeger proto format and forwards them to a kinesis stream
type Exporter struct {
	options   *Options
	producers []*shardProducer
	logger    *zap.Logger
	hooks     *kinesisHooks
	semaphore chan struct{}
}

// Note: We do not implement trace.Exporter interface yet but it is planned
// var _ trace.Exporter = (*Exporter)(nil)

// Flush flushes queues and stops exporters
func (e *Exporter) Flush() {
	for _, sp := range e.producers {
		sp.pr.Stop()
	}
	close(e.semaphore)
}

func (e *Exporter) acquire() {
	if e.semaphore != nil {
		e.semaphore <- struct{}{}
	}
}

func (e *Exporter) release() {
	if e.semaphore != nil {
		<-e.semaphore
	}
}

// ExportSpan exports a Jaeger protbuf span to Kinesis
func (e *Exporter) ExportSpan(span *gen.Span) error {
	return e.ExportJaegerSpan(span)
}

// ExportJaegerSpan exports an OC span to kinesis
func (e *Exporter) ExportJaegerSpan(span *gen.Span) error {
	e.hooks.OnSpanEnqueued()
	e.acquire()
	go e.processJaegerSpan(span)
	return nil
}

// ExportOCSpan exports an OC span to kinesis
func (e *Exporter) ExportOCSpan(span *tracepb.Span) error {
	e.hooks.OnSpanEnqueued()
	e.acquire()
	go e.processOCSpan(span)
	return nil
}

func (e *Exporter) processJaegerSpan(span *gen.Span) {
	defer e.hooks.OnSpanDequeued()
	sp, err := e.getShardProducer(span.TraceID.String())
	if err != nil {
		fmt.Println("failed to get producer/shard for traceID: ", err)
		return
	}
	// todo: see if we can use span.Size() instead
	encoded, err := gogoproto.Marshal(span)
	if err != nil {
		fmt.Println("failed to marshal: ", err)
		return
	}
	size := len(encoded)
	if size > e.options.MaxAllowedSizePerSpan {
		sp.hooks.OnXLSpanDropped(size)
		span.Tags = []gen.KeyValue{
			{Key: "omnition.dropped", VBool: true, VType: gen.ValueType_BOOL},
			{Key: "omnition.dropped.reason", VStr: "unsupported size", VType: gen.ValueType_STRING},
			{Key: "omnition.dropped.size", VInt64: int64(size), VType: gen.ValueType_INT64},
		}
		span.Logs = []gen.Log{}
		encoded, err = gogoproto.Marshal(span)
		if err != nil {
			fmt.Println("failed to modified span: ", err)
			return
		}
		size = len(encoded)
	}
	// TODO: See if we can encode only once and put encoded span on the shard producer.
	// shard producer will have to arrange the bytes exactly as protobuf marshaller would
	// encode a SpanList object.
	// err = sp.pr.Put(encoded, traceID)
	err = sp.putJaeger(span, uint64(size))
	if err != nil {
		fmt.Println("error putting span: ", err)
	}
}

func (e *Exporter) processOCSpan(span *tracepb.Span) {
	defer e.hooks.OnSpanDequeued()
	sp, err := e.getShardProducer(string(span.TraceId))
	if err != nil {
		fmt.Println("failed to get producer/shard for traceID: ", err)
		return
	}
	encoded, err := proto.Marshal(span)
	if err != nil {
		fmt.Println("failed to marshal to OC: ", err)
		return
	}
	size := len(encoded)
	if size > e.options.MaxAllowedSizePerSpan {
		sp.hooks.OnXLSpanDropped(size)
		span.Attributes.AttributeMap = map[string]*tracepb.AttributeValue{
			"omnition.dropped":        &tracepb.AttributeValue{Value: &tracepb.AttributeValue_BoolValue{true}},
			"omnition.dropped.reason": &tracepb.AttributeValue{Value: &tracepb.AttributeValue_StringValue{&tracepb.TruncatableString{Value: "unsupported size"}}},
			"omnition.dropped.size":   &tracepb.AttributeValue{Value: &tracepb.AttributeValue_IntValue{int64(size)}},
		}
		encoded, err = proto.Marshal(span)
		if err != nil {
			fmt.Println("failed to encode modified OC span: ", err)
			return
		}
		size = len(encoded)
	}
	// TODO: See if we can encode only once and put encoded span on the shard producer.
	// shard producer will have to arrange the bytes exactly as protobuf marshaller would
	// encode a SpanList object.
	// err = sp.pr.Put(encoded, traceID)
	err = sp.putOC(span, uint64(size))
	if err != nil {
		fmt.Println("error putting span: ", err)
	}
}

/*
func (e *Exporter) loop() {
	// TODO: Add graceful shutdown
	for {
		// TODO: check all errors and record metrics
		// handle channel closing
		span := <-e.queue
		e.processSpan(span)
	}
}
*/

func (e *Exporter) getShardProducer(partitionKey string) (*shardProducer, error) {
	for _, sp := range e.producers {
		ok, err := sp.shard.belongsToShard(partitionKey)
		if err != nil {
			return nil, err
		}
		if ok {
			return sp, nil
		}
	}
	return nil, fmt.Errorf("no shard found for parition key %s", partitionKey)
}
