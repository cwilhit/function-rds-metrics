package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/crossplane/function-sdk-go/errors"
	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
	"github.com/cwilhit/function-rds-metrics/input/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Function returns RDS metrics from AWS CloudWatch.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log logging.Logger
}

// RDSMetrics represents the metrics data structure
type RDSMetrics struct {
	DatabaseName string                 `json:"databaseName"`
	Region       string                 `json:"region"`
	Timestamp    time.Time              `json:"timestamp"`
	Metrics      map[string]MetricValue `json:"metrics"`
}

// MetricValue represents a single metric value
type MetricValue struct {
	Value     float64   `json:"value"`
	Unit      string    `json:"unit"`
	Timestamp time.Time `json:"timestamp"`
}

// Object represents the metrics result structure
type Object struct {
	Data map[string]MetricValue `json:"data"`
}

// Default metrics to fetch if none specified
var defaultMetrics = []string{
	"CPUUtilization",
	"DatabaseConnections",
	"FreeableMemory",
	"FreeStorageSpace",
	"ReadIOPS",
	"WriteIOPS",
	"ReadLatency",
	"WriteLatency",
}

// RunFunction runs the Function.
func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running RDS metrics function", "tag", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	// Ensure the context is preserved
	f.preserveContext(req, rsp)

	// Parse input and get credentials
	in, awsCreds, err := f.parseInputAndCredentials(req, rsp)
	if err != nil {
		return rsp, nil //nolint:nilerr // errors are handled in rsp. We should not error main function and proceed with reconciliation
	}

	// Validate required inputs
	if in.DatabaseName == "" {
		response.ConditionFalse(rsp, "FunctionSuccess", "InvalidInput").
			WithMessage("DatabaseName is required").
			TargetCompositeAndClaim()
		return rsp, nil
	}

	// Get AWS configuration
	awsConfig, err := f.getAWSConfig(ctx, awsCreds, in.Region)
	if err != nil {
		response.ConditionFalse(rsp, "FunctionSuccess", "AWSConfigError").
			WithMessage(fmt.Sprintf("Failed to create AWS config: %v", err)).
			TargetCompositeAndClaim()
		return rsp, nil
	}

	// Create CloudWatch client
	cwClient := cloudwatch.NewFromConfig(awsConfig)

	// Determine which metrics to fetch
	metricsToFetch := in.Metrics
	if len(metricsToFetch) == 0 {
		metricsToFetch = defaultMetrics
	}

	// Set default period if not specified
	period := in.Period
	if period == 0 {
		period = 300 // 5 minutes default
	}

	// Fetch metrics from CloudWatch
	metricsData, err := f.fetchRDSMetrics(ctx, cwClient, in.DatabaseName, metricsToFetch, period)
	if err != nil {
		response.ConditionFalse(rsp, "FunctionSuccess", "CloudWatchError").
			WithMessage(fmt.Sprintf("Failed to fetch RDS metrics: %v", err)).
			TargetCompositeAndClaim()
		return rsp, nil
	}

	// Create the metrics object
	rdsMetrics := &RDSMetrics{
		DatabaseName: in.DatabaseName,
		Region:       awsConfig.Region,
		Timestamp:    time.Now(),
		Metrics:      metricsData,
	}

	// Convert to unstructured object
	metricsObj := &unstructured.Unstructured{}
	metricsObj.SetAPIVersion("rds-metrics.fn.crossplane.io/v1beta1")
	metricsObj.SetKind("RDSMetrics")
	metricsObj.SetName(fmt.Sprintf("%s-metrics", in.DatabaseName))

	// Convert metrics to JSON and set as object
	metricsJSON, err := json.Marshal(rdsMetrics)
	if err != nil {
		response.ConditionFalse(rsp, "FunctionSuccess", "SerializationError").
			WithMessage(fmt.Sprintf("Failed to serialize metrics: %v", err)).
			TargetCompositeAndClaim()
		return rsp, nil
	}

	var metricsMap map[string]interface{}
	if err := json.Unmarshal(metricsJSON, &metricsMap); err != nil {
		response.ConditionFalse(rsp, "FunctionSuccess", "SerializationError").
			WithMessage(fmt.Sprintf("Failed to deserialize metrics: %v", err)).
			TargetCompositeAndClaim()
		return rsp, nil
	}

	metricsObj.Object = metricsMap

	err = f.putMetricsResultToStatus(req, rsp, in, rdsMetrics)
	if err != nil {
		response.ConditionFalse(rsp, "FunctionSuccess", "SerializationError").
			WithMessage(fmt.Sprintf("Failed to put metrics result to status: %v", err)).
			TargetCompositeAndClaim()
		return rsp, nil
	}

	response.ConditionTrue(rsp, "FunctionSuccess", "Success").
		WithMessage(fmt.Sprintf("Successfully fetched metrics for RDS instance %s", in.DatabaseName)).
		TargetCompositeAndClaim()

	f.log.Info("Successfully fetched RDS metrics", "database", in.DatabaseName, "region", awsConfig.Region)

	return rsp, nil
}

// getXRAndStatus retrieves status and desired XR, handling initialization if needed
func (f *Function) getXRAndStatus(req *fnv1.RunFunctionRequest) (map[string]interface{}, *resource.Composite, error) {
	// Get both observed and desired XR
	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot get observed composite resource")
	}

	dxr, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot get desired composite resource")
	}

	xrStatus := make(map[string]interface{})

	// Initialize dxr from oxr if needed
	if dxr.Resource.GetKind() == "" {
		dxr.Resource.SetAPIVersion(oxr.Resource.GetAPIVersion())
		dxr.Resource.SetKind(oxr.Resource.GetKind())
		dxr.Resource.SetName(oxr.Resource.GetName())
	}

	// First try to get status from desired XR (pipeline changes)
	if dxr.Resource.GetKind() != "" {
		err = dxr.Resource.GetValueInto("status", &xrStatus)
		if err == nil && len(xrStatus) > 0 {
			return xrStatus, dxr, nil
		}
		f.log.Debug("Cannot get status from Desired XR or it's empty")
	}

	// Fallback to observed XR status
	err = oxr.Resource.GetValueInto("status", &xrStatus)
	if err != nil {
		f.log.Debug("Cannot get status from Observed XR")
	}

	return xrStatus, dxr, nil
}

// ParseNestedKey enables the bracket and dot notation to key reference
func ParseNestedKey(key string) ([]string, error) {
	var parts []string
	// Regular expression to extract keys, supporting both dot and bracket notation
	regex := regexp.MustCompile(`\[([^\[\]]+)\]|([^.\[\]]+)`)
	matches := regex.FindAllStringSubmatch(key, -1)
	for _, match := range matches {
		if match[1] != "" {
			parts = append(parts, match[1]) // Bracket notation
		} else if match[2] != "" {
			parts = append(parts, match[2]) // Dot notation
		}
	}

	if len(parts) == 0 {
		return nil, errors.New("invalid key")
	}
	return parts, nil
}

// SetNestedKey sets a value to a nested key from a map using dot notation keys.
func SetNestedKey(root map[string]interface{}, key string, value interface{}) error {
	parts, err := ParseNestedKey(key)
	if err != nil {
		return err
	}

	current := root
	for i, part := range parts {
		if i == len(parts)-1 {
			// Set the value at the final key
			current[part] = value
			return nil
		}

		// Traverse into nested maps or create them if they don't exist
		if next, exists := current[part]; exists {
			if nextMap, ok := next.(map[string]interface{}); ok {
				current = nextMap
			} else {
				return fmt.Errorf("key %q exists but is not a map", part)
			}
		} else {
			// Create a new map if the path doesn't exist
			newMap := make(map[string]interface{})
			current[part] = newMap
			current = newMap
		}
	}

	return nil
}

// putMetricsResultToStatus processes the metrics results to status
func (f *Function) putMetricsResultToStatus(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse, in *v1beta1.Input, results *RDSMetrics) error {
	xrStatus, dxr, err := f.getXRAndStatus(req)
	if err != nil {
		return err
	}

	// Prepare the result data
	resultData := results

	// Update the specific status field
	statusField := strings.TrimPrefix(in.Target, "status.")
	err = SetNestedKey(xrStatus, statusField, resultData)
	if err != nil {
		return errors.Wrapf(err, "cannot set status field %s to %v", statusField, resultData)
	}

	// Write the updated status field back into the composite resource
	if err := dxr.Resource.SetValue("status", xrStatus); err != nil {
		return errors.Wrap(err, "cannot write updated status back into composite resource")
	}

	// Save the updated desired composite resource
	if err := response.SetDesiredCompositeResource(rsp, dxr); err != nil {
		return errors.Wrapf(err, "cannot set desired composite resource in %T", rsp)
	}
	return nil
}

func getCreds(req *fnv1.RunFunctionRequest) (map[string]string, error) {
	var awsCreds map[string]string
	rawCreds := req.GetCredentials()

	if credsData, ok := rawCreds["aws-creds"]; ok {
		credsMap := credsData.GetCredentialData().GetData()
		awsCreds = make(map[string]string)
		for k, v := range credsMap {
			awsCreds[k] = string(v)
		}
	} else {
		return nil, errors.New("failed to get aws-creds credentials")
	}

	return awsCreds, nil
}

// parseInputAndCredentials parses the input and gets the credentials.
func (f *Function) parseInputAndCredentials(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse) (*v1beta1.Input, map[string]string, error) {
	in := &v1beta1.Input{}
	if err := request.GetInput(req, in); err != nil {
		response.ConditionFalse(rsp, "FunctionSuccess", "InternalError").
			WithMessage("Something went wrong.").
			TargetCompositeAndClaim()

		response.Warning(rsp, errors.New("something went wrong")).
			TargetCompositeAndClaim()

		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return nil, nil, err
	}

	awsCreds, err := getCreds(req)
	if err != nil {
		response.Fatal(rsp, err)
		return nil, nil, err
	}

	return in, awsCreds, nil
}

// preserveContext ensures the context is preserved in the response
func (f *Function) preserveContext(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse) {
	// Get the existing context from the request
	existingContext := req.GetContext()
	if existingContext != nil {
		// Copy the existing context to the response
		rsp.Context = existingContext
		f.log.Info("Preserved existing context in response")
	}
}

// getAWSConfig creates AWS configuration from the provided credentials
func (f *Function) getAWSConfig(ctx context.Context, awsCreds map[string]string, region string) (aws.Config, error) {
	// Extract credentials from the provided map
	accessKeyID, ok := awsCreds["access-key-id"]
	if !ok {
		return aws.Config{}, fmt.Errorf("access-key-id not found in credentials")
	}

	secretAccessKey, ok := awsCreds["secret-access-key"]
	if !ok {
		return aws.Config{}, fmt.Errorf("secret-access-key not found in credentials")
	}

	// Use the region from input, with default fallback
	if region == "" {
		region = "us-east-1" // Default region
	}

	// Create AWS config with static credentials
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     accessKeyID,
				SecretAccessKey: secretAccessKey,
			}, nil
		})),
	)
	if err != nil {
		return aws.Config{}, fmt.Errorf("failed to create AWS config: %w", err)
	}

	return cfg, nil
}

// fetchRDSMetrics fetches RDS metrics from CloudWatch
func (f *Function) fetchRDSMetrics(ctx context.Context, client *cloudwatch.Client, dbName string, metrics []string, period int32) (map[string]MetricValue, error) {
	metricsData := make(map[string]MetricValue)
	endTime := time.Now()
	startTime := endTime.Add(-time.Duration(period) * time.Second)

	for _, metricName := range metrics {
		input := &cloudwatch.GetMetricStatisticsInput{
			Namespace:  aws.String("AWS/RDS"),
			MetricName: aws.String(metricName),
			Dimensions: []types.Dimension{
				{
					Name:  aws.String("DBInstanceIdentifier"),
					Value: aws.String(dbName),
				},
			},
			StartTime: aws.Time(startTime),
			EndTime:   aws.Time(endTime),
			Period:    aws.Int32(60),
			Statistics: []types.Statistic{
				types.StatisticSampleCount,
			},
		}

		result, err := client.GetMetricStatistics(ctx, input)
		if err != nil {
			f.log.Info("Failed to fetch metric", "metric", metricName, "error", err)
			continue
		}

		if len(result.Datapoints) > 0 {
			// Get the most recent datapoint
			latest := result.Datapoints[0]
			for _, dp := range result.Datapoints {
				if dp.Timestamp.After(*latest.Timestamp) {
					latest = dp
				}
			}

			metricsData[metricName] = MetricValue{
				Value:     *latest.Average,
				Unit:      string(latest.Unit),
				Timestamp: *latest.Timestamp,
			}
		}
	}

	return metricsData, nil
}
