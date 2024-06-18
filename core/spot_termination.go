// Copyright (c) 2016-2022 Cristian Măgherușan-Stanciu
// Licensed under the Open Software License version 3.0

package autospotting

import (
	"errors"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

const (
	// DefaultTerminationNotificationAction is the default value for the termination notification
	// action configuration option
	DefaultTerminationNotificationAction = AutoTerminationNotificationAction
)

//SpotTermination is used to detach an instance, used when a spot instance is due for termination
type SpotTermination struct {
	asSvc           autoscalingiface.AutoScalingAPI
	ec2Svc          ec2iface.EC2API
	SleepMultiplier time.Duration
	asg             autoScalingGroup
}

func newSpotTermination(region string) SpotTermination {

	log.Println("Connection to region ", region)

	session := session.Must(
		session.NewSession(&aws.Config{Region: aws.String(region)}))

	return SpotTermination{

		asSvc:           autoscaling.New(session),
		ec2Svc:          ec2.New(session),
		SleepMultiplier: 1,
	}
}

//DetachInstance detaches the instance from autoscaling group without decrementing the desired capacity
//This makes sure that the autoscaling group spawns a new instance as soon as this instance is detached
func (s *SpotTermination) detachInstance(instanceID *string, asgName string, eventType string) error {

	log.Println(asgName,
		"Detaching instance:",
		*instanceID)

	detachParams := autoscaling.DetachInstancesInput{
		AutoScalingGroupName: aws.String(asgName),
		InstanceIds: []*string{
			instanceID,
		},
		ShouldDecrementDesiredCapacity: aws.Bool(false),
	}
	if _, detachErr := s.asSvc.DetachInstances(&detachParams); detachErr != nil {
		log.Println(detachErr.Error())
		return detachErr
	}

	log.Printf("Detached instance %s successfully", *instanceID)

	if eventType != InstanceRebalanceRecommendationCode {
		s.deleteTagInstanceLaunchedForAsg(instanceID)
		s.delayedTermination(instanceID, 14)
	}

	return nil
}

// delayedTermination is used to terminate instances that were marked as being in danger of being terminated.
func (s *SpotTermination) delayedTermination(instanceID *string, minutes time.Duration) error {

	log.Printf("Terminating instance %s with %d minutes delay, sleeping...\n",
		*instanceID, minutes)

	time.Sleep(minutes * time.Minute * s.SleepMultiplier)

	log.Println("Terminating instance", *instanceID)
	// terminate the spot instance
	terminateParams := ec2.TerminateInstancesInput{
		InstanceIds: []*string{instanceID},
	}

	if _, err := s.ec2Svc.TerminateInstances(&terminateParams); err != nil {
		log.Println(err.Error())
		return err
	}
	return nil
}

//TerminateInstance terminate the instance from autoscaling group without decrementing the desired capacity
//This makes sure that any LifeCycle Hook configured is triggered and the autoscaling group spawns a new instance
// as soon as this instance begin terminating.
func (s *SpotTermination) terminateInstance(instanceID *string, asgName string) error {

	log.Println(asgName,
		"Terminating instance:",
		*instanceID)
	// terminate the spot instance
	terminateParams := autoscaling.TerminateInstanceInAutoScalingGroupInput{
		InstanceId:                     instanceID,
		ShouldDecrementDesiredCapacity: aws.Bool(false),
	}

	if _, err := s.asSvc.TerminateInstanceInAutoScalingGroup(&terminateParams); err != nil {
		log.Println(err.Error())
		return err
	}
	return nil
}

func (s *SpotTermination) getAsgName(instanceID *string) (string, error) {
	asParams := autoscaling.DescribeAutoScalingInstancesInput{
		InstanceIds: []*string{instanceID},
	}

	result, err := s.asSvc.DescribeAutoScalingInstances(&asParams)
	if err != nil {
		return "", err
	} else if len(result.AutoScalingInstances) == 0 {
		return "", nil
	}

	return *result.AutoScalingInstances[0].AutoScalingGroupName, nil
}

// ExecuteAction execute the proper termination action (terminate|detach) based on the value of
// terminationNotificationAction and the presence of a LifecycleHook on ASG.
func (s *SpotTermination) executeAction(instanceID *string, terminationNotificationAction string, eventType string) error {
	if s.asSvc == nil {
		return errors.New("AutoScaling service not defined. Please use NewSpotTermination()")
	}

	asgName, err := s.getAsgName(instanceID)

	if err != nil {
		log.Printf("Failed get ASG name for %s with err: %s\n", *instanceID, err.Error())
		return err
	} else if asgName == "" {
		log.Println("Instance", instanceID, "does not belong to an autoscaling group")
		return nil
	}

	switch terminationNotificationAction {
	case "detach":
		s.detachInstance(instanceID, asgName, eventType)
	case "terminate":
		s.terminateInstance(instanceID, asgName)
	default:
		if s.asgHasTerminationLifecycleHook(&asgName) {
			s.terminateInstance(instanceID, asgName)
		} else {
			s.detachInstance(instanceID, asgName, eventType)
		}
	}

	return nil
}

func (s *SpotTermination) deleteTagInstanceLaunchedForAsg(instanceID *string) error {
	ec2Params := ec2.DeleteTagsInput{
		Resources: []*string{
			aws.String(*instanceID),
		},
		Tags: []*ec2.Tag{
			{
				Key: aws.String("launched-for-asg"),
			},
		},
	}
	_, err := s.ec2Svc.DeleteTags(&ec2Params)

	if err != nil {
		log.Printf("Failed to delete Tag 'launched-for-asg' from spot instance %s with err: %s\n", *instanceID, err.Error())
		return err
	}

	log.Printf("Tag 'launched-for-asg' deleted from spot instance %s", *instanceID)

	return nil
}

func (s *SpotTermination) asgHasTerminationLifecycleHook(autoScalingGroupName *string) bool {
	asParams := autoscaling.DescribeLifecycleHooksInput{
		AutoScalingGroupName: autoScalingGroupName,
	}

	result, err := s.asSvc.DescribeLifecycleHooks(&asParams)

	if err != nil {
		log.Println(err.Error())
		return false
	}

	var hasHook = false
	for _, lfh := range result.LifecycleHooks {
		if *lfh.LifecycleTransition == "autoscaling:EC2_INSTANCE_TERMINATING" {
			hasHook = true
			log.Println("Found Hook", *lfh.LifecycleHookName)
			break
		}
	}

	return hasHook
}

// IsInAutoSpottingASG checks to see whether an instance is in an AutoSpotting ASG as defined by its tags.
// If the ASG does not have the required tags, it is not an AutoSpotting ASG and should be left alone.
func (s *SpotTermination) IsInAutoSpottingASG(instanceID *string, tagFilteringMode string, filterByTags string) bool {
	var optInFilterMode = (tagFilteringMode != "opt-out")

	asgName, err := s.getAsgName(instanceID)

	if err != nil {
		log.Printf("Failed get ASG name for %s with err: %s\n", *instanceID, err.Error())
		return false
	} else if asgName == "" {
		log.Println("Instance", *instanceID, "is not in an autoscaling group")
		return false
	}

	asgGroupsOutput, err := s.asSvc.DescribeAutoScalingGroups(&autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{
			&asgName,
		},
	})

	if err != nil {
		log.Printf("Failed to get ASG using ASG name %s with err: %s\n", asgName, err.Error())
		return false
	}

	s.asg = autoScalingGroup{
	  Group:  asgGroupsOutput.AutoScalingGroups[0],
	  name:   asgName,
	}

	filters := replaceWhitespace(filterByTags)

	var tagsToMatch = []Tag{}

	for _, tagWithValue := range strings.Split(filters, ",") {
		tag := splitTagAndValue(tagWithValue)
		if tag != nil {
			tagsToMatch = append(tagsToMatch, *tag)
		}
	}

	isInASG := optInFilterMode == isASGWithMatchingTags(asgGroupsOutput.AutoScalingGroups[0], tagsToMatch)

	if !isInASG {
		log.Printf("Skipping group %s because its tags, the currently "+
			"configured filtering mode (%s) and tag filters do not align\n",
			asgName, tagFilteringMode)
	}

	return isInASG
}


// get AutoscalingGroup config TerminationNotificationAction from Tags
func (s *SpotTermination) getTermAction(defaultTerminationNotificationAction string) string {
  a := s.asg

  tagValue := a.getTagValue(TerminationNotificationActionTag)
  if tagValue != nil {
    log.Printf("Loaded TerminationNotificationAction value %v from tag %v\n", *tagValue, TerminationNotificationActionTag)
    return *tagValue
  }

  debug.Println("Couldn't find tag", TerminationNotificationActionTag, "on the group", a.name, "using the default configuration")
  return defaultTerminationNotificationAction
}
