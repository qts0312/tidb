// Copyright 2024 PingCAP, Inc.
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

package priorityqueue

import (
	"fmt"
	"time"

	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/sysproctrack"
	"github.com/pingcap/tidb/pkg/statistics/handle/logutil"
	statstypes "github.com/pingcap/tidb/pkg/statistics/handle/types"
	"go.uber.org/zap"
)

// defaultFailedAnalysisWaitTime is the default wait time for the next analysis after a failed analysis.
// NOTE: this is only used when the average analysis duration is not available.(No successful analysis before)
const defaultFailedAnalysisWaitTime = 30 * time.Minute

type analyzeType string

// Indicators contains some indicators to evaluate the table priority.
type Indicators struct {
	// ChangePercentage is the percentage of the changed rows.
	// Usually, the more the changed rows, the higher the priority.
	// It is calculated by modifiedCount / last time analysis count.
	ChangePercentage float64
	// TableSize is the table size in rows * len(columns).
	TableSize float64
	// LastAnalysisDuration is the duration from the last analysis to now.
	LastAnalysisDuration time.Duration
}

// SuccessJobHook is the successHook function that will be called after the job is completed.
type SuccessJobHook func(job AnalysisJob)

// FailureJobHook is the failureHook function that will be called after the job is failed.
type FailureJobHook func(job AnalysisJob, mustRetry bool)

// AnalysisJob is the interface for the analysis job.
type AnalysisJob interface {
	// ValidateAndPrepare validates if the analysis job can run and prepares it for execution.
	// Returns (true, "") if the job is valid and ready to run.
	// Returns (false, reason) if the job should be skipped, where reason explains why:
	// - Schema/table/partition doesn't exist
	// - Table is not partitioned (for partition jobs)
	// - Recent failed analysis (within 2x avg duration) to avoid queue blocking
	ValidateAndPrepare(
		sctx sessionctx.Context,
	) (bool, string)

	// Analyze executes the analyze statement within a transaction.
	Analyze(
		statsHandle statstypes.StatsHandle,
		sysProcTracker sysproctrack.Tracker,
	) error

	// SetWeight sets the weight of the job.
	SetWeight(weight float64)

	// GetWeight gets the weight of the job.
	GetWeight() float64

	// HasNewlyAddedIndex checks whether the job has newly added index.
	HasNewlyAddedIndex() bool

	// GetIndicators gets the indicators of the job.
	GetIndicators() Indicators

	// SetIndicators sets the indicators of the job.
	SetIndicators(indicators Indicators)

	// GetTableID gets the table ID of the job.
	GetTableID() int64

	// RegisterSuccessHook registers a successHook function that will be called after the job can be marked as successful.
	RegisterSuccessHook(hook SuccessJobHook)

	// RegisterFailureHook registers a failureHook function that will be called after the job is marked as failed.
	RegisterFailureHook(hook FailureJobHook)

	// AsJSON converts the job to a JSON object.
	AsJSON() statstypes.AnalysisJobJSON

	fmt.Stringer
}

const (
	schemaNotExist      = "schema does not exist"
	tableNotExist       = "table does not exist"
	notPartitionedTable = "table is not a partitioned table"
	partitionNotExist   = "partition does not exist"
)

// isValidToAnalyze checks whether the table is valid to analyze.
// It checks the last failed analysis duration and the average analysis duration.
// If the last failed analysis duration is less than 2 times the average analysis duration,
// we skip this table to avoid too much failed analysis. Because the last analysis just failed,
// we don't want to block the queue by analyzing it again.
func isValidToAnalyze(
	sctx sessionctx.Context,
	schema, table string,
	partitionNames ...string,
) (bool, string) {
	lastFailedAnalysisDuration, err :=
		GetLastFailedAnalysisDuration(sctx, schema, table, partitionNames...)
	if err != nil {
		logutil.StatsErrVerboseSampleLogger().Warn(
			"Fail to get last failed analysis duration",
			zap.String("schema", schema),
			zap.String("table", table),
			zap.Strings("partitions", partitionNames),
			zap.Error(err),
		)
		return false, fmt.Sprintf("fail to get last failed analysis duration: %v", err)
	}

	averageAnalysisDuration, err :=
		GetAverageAnalysisDuration(sctx, schema, table, partitionNames...)
	if err != nil {
		logutil.StatsErrVerboseSampleLogger().Warn(
			"Fail to get average analysis duration",
			zap.String("schema", schema),
			zap.String("table", table),
			zap.Strings("partitions", partitionNames),
			zap.Error(err),
		)
		return false, fmt.Sprintf("fail to get average analysis duration: %v", err)
	}

	// Last analysis just failed, we should not analyze it again.
	if lastFailedAnalysisDuration == justFailed {
		// The last analysis failed, we should not analyze it again.
		logutil.StatsSampleLogger().Info(
			"Skip analysis because the last analysis just failed",
			zap.String("schema", schema),
			zap.String("table", table),
			zap.Strings("partitions", partitionNames),
		)
		return false, "last analysis just failed"
	}

	// Failed analysis duration is less than 2 times the average analysis duration.
	// Skip this table to avoid too much failed analysis.
	onlyFailedAnalysis := lastFailedAnalysisDuration != NoRecord && averageAnalysisDuration == NoRecord
	if onlyFailedAnalysis && lastFailedAnalysisDuration < defaultFailedAnalysisWaitTime {
		logutil.StatsSampleLogger().Info(
			fmt.Sprintf("Skip analysis because the last failed analysis duration is less than %v", defaultFailedAnalysisWaitTime),
			zap.String("schema", schema),
			zap.String("table", table),
			zap.Strings("partitions", partitionNames),
			zap.Duration("lastFailedAnalysisDuration", lastFailedAnalysisDuration),
			zap.Duration("averageAnalysisDuration", averageAnalysisDuration),
		)
		return false, fmt.Sprintf("last failed analysis duration is less than %v", defaultFailedAnalysisWaitTime)
	}
	// Failed analysis duration is less than 2 times the average analysis duration.
	meetSkipCondition := lastFailedAnalysisDuration != NoRecord &&
		lastFailedAnalysisDuration < 2*averageAnalysisDuration
	if meetSkipCondition {
		logutil.StatsSampleLogger().Info(
			"Skip analysis because the last failed analysis duration is less than 2 times the average analysis duration",
			zap.String("schema", schema),
			zap.String("table", table),
			zap.Strings("partitions", partitionNames),
			zap.Duration("lastFailedAnalysisDuration", lastFailedAnalysisDuration),
			zap.Duration("averageAnalysisDuration", averageAnalysisDuration),
		)
		return false, "last failed analysis duration is less than 2 times the average analysis duration"
	}

	return true, ""
}

// IsDynamicPartitionedTableAnalysisJob checks whether the job is a dynamic partitioned table analysis job.
func IsDynamicPartitionedTableAnalysisJob(job AnalysisJob) bool {
	_, ok := job.(*DynamicPartitionedTableAnalysisJob)
	return ok
}

// asJSONIndicators converts the indicators to a JSON object.
func asJSONIndicators(indicators Indicators) statstypes.IndicatorsJSON {
	return statstypes.IndicatorsJSON{
		ChangePercentage:     fmt.Sprintf("%.2f%%", indicators.ChangePercentage*100),
		TableSize:            fmt.Sprintf("%.2f", indicators.TableSize),
		LastAnalysisDuration: fmt.Sprintf("%v", indicators.LastAnalysisDuration),
	}
}
