// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package executer allows execute Pending association and InProgress association
package executer

import (
	"fmt"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/association/schedulemanager"
	"github.com/aws/amazon-ssm-agent/agent/association/schedulemanager/signal"
	"github.com/aws/amazon-ssm-agent/agent/association/service"
	"github.com/aws/amazon-ssm-agent/agent/association/taskpool"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/framework/plugin"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/reply"
	stateModel "github.com/aws/amazon-ssm-agent/agent/statemanager/model"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/amazon-ssm-agent/agent/times"
	"github.com/aws/aws-sdk-go/service/ssm"
)

const (
	outputMessageTemplate  string = "%v out of %v plugin%v processed, %v success, %v failed, %v timedout"
	documentPendingMessage string = "Association is pending"
)

// DocumentExecuter represents the interface for running a document
type DocumentExecuter interface {
	ExecutePendingDocument(context context.T, pool taskpool.T, docState *stateModel.DocumentState) error
	ExecuteInProgressDocument(context context.T, docState *stateModel.DocumentState, cancelFlag task.CancelFlag)
}

// AssociationExecuter represents the implementation of document executer
type AssociationExecuter struct {
	assocSvc  service.T
	agentInfo *contracts.AgentInfo
}

// NewAssociationExecuter returns a new document executer
func NewAssociationExecuter(assocSvc service.T, agentInfo *contracts.AgentInfo) *AssociationExecuter {
	runner := AssociationExecuter{
		assocSvc:  assocSvc,
		agentInfo: agentInfo,
	}

	return &runner
}

// ExecutePendingDocument moves doc to current folder and submit it for execution
func (r *AssociationExecuter) ExecutePendingDocument(context context.T, pool taskpool.T, docState *stateModel.DocumentState) error {
	log := context.With("[associationId=" + docState.DocumentInformation.AssociationID + "]").Log()
	log.Debugf("Persist document and update association status to pending")

	r.assocSvc.UpdateInstanceAssociationStatus(
		log,
		docState.DocumentInformation.AssociationID,
		docState.DocumentInformation.DocumentName,
		docState.DocumentInformation.InstanceID,
		contracts.AssociationStatusPending,
		contracts.AssociationErrorCodeNoError,
		times.ToIso8601UTC(time.Now()),
		documentPendingMessage)

	bookkeepingSvc.MoveDocumentState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfPending,
		appconfig.DefaultLocationOfCurrent)

	if err := pool.Submit(log, docState.DocumentInformation.AssociationID, func(cancelFlag task.CancelFlag) {
		r.ExecuteInProgressDocument(context, docState, cancelFlag)
	}); err != nil {
		return fmt.Errorf("failed to process association, %v", err)
	}

	return nil
}

// ExecuteInProgressDocument parses and processes the document
func (r *AssociationExecuter) ExecuteInProgressDocument(context context.T, docState *stateModel.DocumentState, cancelFlag task.CancelFlag) {
	assocContext := context.With("[associationId=" + docState.DocumentInformation.AssociationID + "]")
	log := assocContext.Log()

	defer func() {
		schedulemanager.UpdateNextScheduledDate(log, docState.DocumentInformation.AssociationID)
		signal.ExecuteAssociation(log)
	}()

	totalNumberOfActions := len(docState.InstancePluginsInformation)
	outputs := pluginExecution.RunPlugins(
		assocContext,
		docState.DocumentInformation.AssociationID,
		docState.DocumentInformation.CreatedDate,
		docState.InstancePluginsInformation,
		plugin.RegisteredWorkerPlugins(assocContext),
		r.pluginExecutionReport,
		cancelFlag)

	pluginOutputContent, err := jsonutil.Marshal(outputs)
	if err != nil {
		log.Error("failed to parse to json string ", err)
		return
	}
	log.Debugf("Plugin outputs %v", jsonutil.Indent(pluginOutputContent))

	r.parseAndPersistReplyContents(log, docState, outputs)
	// Skip sending response when the document requires a reboot
	if docState.IsRebootRequired() {
		log.Debugf("skipping sending response of %v since the document requires a reboot", docState.DocumentInformation.AssociationID)
		return
	}

	if pluginOutputContent, err = jsonutil.Marshal(outputs); err != nil {
		log.Error("failed to parse to json string ", err)
		return
	}

	log.Debug("Association execution completion ", pluginOutputContent)
	log.Debug("Association execution status is ", docState.DocumentInformation.DocumentStatus)
	if docState.DocumentInformation.DocumentStatus == contracts.ResultStatusFailed {
		r.associationExecutionReport(
			log,
			&docState.DocumentInformation,
			docState.DocumentInformation.RuntimeStatus,
			totalNumberOfActions,
			contracts.AssociationErrorCodeExecutionError,
			ssm.AssociationStatusNameFailed)

	} else if docState.DocumentInformation.DocumentStatus == contracts.ResultStatusSuccess {
		r.associationExecutionReport(
			log,
			&docState.DocumentInformation,
			docState.DocumentInformation.RuntimeStatus,
			totalNumberOfActions,
			contracts.AssociationErrorCodeNoError,
			contracts.AssociationStatusSuccess)
	}

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("execution of %v is over. Moving docState file from Current to Completed folder", docState.DocumentInformation.AssociationID)
	bookkeepingSvc.MoveDocumentState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)
}

// parseAndPersistReplyContents reloads interimDocState, updates it with replyPayload and persist it on disk.
func (r *AssociationExecuter) parseAndPersistReplyContents(log log.T,
	docState *stateModel.DocumentState,
	pluginOutputs map[string]*contracts.PluginResult) {

	//update interim cmd state file
	docState.DocumentInformation = bookkeepingSvc.GetDocumentInfo(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent)

	runtimeStatuses := reply.PrepareRuntimeStatuses(log, pluginOutputs)
	replyPayload := reply.PrepareReplyPayload("", runtimeStatuses, time.Now(), *r.agentInfo, false)

	// set document level information which wasn't set previously
	docState.DocumentInformation.AdditionalInfo = replyPayload.AdditionalInfo
	docState.DocumentInformation.DocumentStatus = replyPayload.DocumentStatus
	docState.DocumentInformation.DocumentTraceOutput = replyPayload.DocumentTraceOutput
	docState.DocumentInformation.RuntimeStatus = replyPayload.RuntimeStatus

	//persist final documentInfo.
	bookkeepingSvc.PersistDocumentInfo(log,
		docState.DocumentInformation,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent)
}

// pluginExecutionReport allow engine to update progress after every plugin execution
// TODO: documentCreatedDate is not used, remove it from the method
func (r *AssociationExecuter) pluginExecutionReport(
	log log.T,
	associationID string,
	documentCreatedDate string,
	pluginOutputs map[string]*contracts.PluginResult,
	totalNumberOfPlugins int) {

	outputContent, err := jsonutil.Marshal(pluginOutputs)
	if err != nil {
		log.Error("could not marshal plugin outputs! ", err)
		return
	}
	log.Info("Update instance association status with results ", jsonutil.Indent(outputContent))

	// Legacy association api does not support plugin level status update
	// it returns error for multiple update with same status
	if !r.assocSvc.IsInstanceAssociationApiMode() {
		return
	}

	instanceID, err := platform.InstanceID()
	if err != nil {
		log.Error("failed to load instance id ", err)
		return
	}

	runtimeStatuses := reply.PrepareRuntimeStatuses(log, pluginOutputs)
	executionSummary := buildOutput(runtimeStatuses, totalNumberOfPlugins)

	r.assocSvc.UpdateInstanceAssociationStatus(
		log,
		associationID,
		"",
		instanceID,
		contracts.AssociationStatusInProgress,
		contracts.AssociationErrorCodeNoError,
		times.ToIso8601UTC(time.Now()),
		executionSummary)
}

// associationExecutionReport update the status for association
func (r *AssociationExecuter) associationExecutionReport(
	log log.T,
	docInfo *stateModel.DocumentInfo,
	runtimeStatuses map[string]*contracts.PluginRuntimeStatus,
	totalNumberOfPlugins int,
	errorCode string,
	associationStatus string) {

	runtimeStatusesContent, err := jsonutil.Marshal(runtimeStatuses)
	if err != nil {
		log.Error("could not marshal plugin outputs ", err)
		return
	}
	log.Info("Update instance association status with results ", jsonutil.Indent(runtimeStatusesContent))

	executionSummary := buildOutput(runtimeStatuses, totalNumberOfPlugins)
	r.assocSvc.UpdateInstanceAssociationStatus(
		log,
		docInfo.AssociationID,
		docInfo.DocumentName,
		docInfo.InstanceID,
		associationStatus,
		errorCode,
		times.ToIso8601UTC(time.Now()),
		executionSummary)
}

// buildOutput build the output message for association update
func buildOutput(runtimeStatuses map[string]*contracts.PluginRuntimeStatus, totalNumberOfPlugins int) string {
	plural := ""
	if totalNumberOfPlugins > 1 {
		plural = "s"
	}

	completed := len(filterByStatus(runtimeStatuses, func(status contracts.ResultStatus) bool {
		return status != ""
	}))

	success := len(filterByStatus(runtimeStatuses, func(status contracts.ResultStatus) bool {
		return status == contracts.ResultStatusPassedAndReboot ||
			status == contracts.ResultStatusSuccessAndReboot ||
			status == contracts.ResultStatusSuccess
	}))
	failed := len(filterByStatus(runtimeStatuses, func(status contracts.ResultStatus) bool {
		return status == contracts.ResultStatusFailed
	}))
	timedOut := len(filterByStatus(runtimeStatuses, func(status contracts.ResultStatus) bool {
		return status == contracts.ResultStatusTimedOut
	}))

	return fmt.Sprintf(outputMessageTemplate, completed, totalNumberOfPlugins, plural, success, failed, timedOut)
}

// filterByStatus represents the helper method that filter pluginResults base on ResultStatus
func filterByStatus(runtimeStatuses map[string]*contracts.PluginRuntimeStatus, predicate func(contracts.ResultStatus) bool) map[string]*contracts.PluginRuntimeStatus {
	result := make(map[string]*contracts.PluginRuntimeStatus)
	for name, value := range runtimeStatuses {
		if predicate(value.Status) {
			result[name] = value
		}
	}
	return result
}
