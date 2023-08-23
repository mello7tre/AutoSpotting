// Copyright (c) 2016-2022 Cristian Măgherușan-Stanciu
// Licensed under the Open Software License version 3.0

package autospotting

import (
	"errors"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// instance_actions.go contains functions that act on instances, altering their state.

func (i *instance) handleInstanceStates() (bool, error) {
	log.Printf("%s Found instance %s in state %s",
		i.region.name, *i.InstanceId, *i.State.Name)

	if *i.State.Name != "running" {
		log.Printf("%s Instance %s is not in the running state",
			i.region.name, *i.InstanceId)
		return true, errors.New("instance not in running state")
	}

	unattached := i.isUnattachedSpotInstanceLaunchedForAnEnabledASG()
	if !unattached {
		log.Printf("%s Instance %s is already attached to an ASG, skipping it",
			i.region.name, *i.InstanceId)
		return true, nil
	}
	return false, nil
}

// returns an instance ID or error
func (i *instance) launchSpotReplacement() (*string, error) {

	ltData, err := i.createLaunchTemplateData()

	debug.Printf("Launch template data: %+#v", ltData)

	if err != nil {
		log.Println("failed to create LaunchTemplate data,", err.Error())
		return nil, err
	}

	lt, err := i.createFleetLaunchTemplate(ltData)

	debug.Printf("Fleet Launch Template: %+#v", lt)

	if err != nil {
		log.Println(i.region.name, i.asg.name, "createFleetLaunchTemplate() failure:", err.Error())
		return nil, err
	}

	defer i.deleteLaunchTemplate(lt)
	instanceTypes, err := i.getCompatibleSpotInstanceTypesListSortedAscendingByPrice(
		i.asg.getAllowedInstanceTypes(i),
		i.asg.getDisallowedInstanceTypes(i))

	if err != nil {
		log.Println("Couldn't determine the list of compatible spot instance types")
		return nil, err
	}

	cfi := i.createFleetInput(lt, instanceTypes)

	debug.Printf("Fleet Input: %+#v", cfi)

	resp, err := i.region.services.ec2.CreateFleet(cfi)

	if err != nil {
		log.Println(i.region.name, i.asg.name, "CreateFleet() failure:", err.Error())
		return nil, err
	}

	if resp != nil && len(resp.Instances) > 0 && resp.Instances[0] != nil && len(resp.Instances[0].InstanceIds) > 0 {
		return resp.Instances[0].InstanceIds[0], nil
	}

	if resp != nil && len(resp.Errors) > 0 {
		log.Println(i.region.name, i.asg.name, "CreateFleet, instances cannot be launched:", resp.Errors)
	}

	return nil, fmt.Errorf("Couldn't launch spot instance replacement")
}

func (i *instance) swapWithGroupMember(asg *autoScalingGroup) (*instance, error) {

	odInstance, err := i.getSwapCandidate()
	if err != nil {
		log.Printf("Couldn't find suitable OnDemand swap candidate: %s", err.Error())
		return nil, err
	}

	asg.suspendProcesses()
	defer asg.resumeProcesses()

	desiredCapacity, maxSize := *asg.DesiredCapacity, *asg.MaxSize

	// temporarily increase AutoScaling group in case the desired capacity reaches the max size,
	// otherwise attachSpotInstance might fail
	if desiredCapacity == maxSize {
		log.Println(asg.name, "Temporarily increasing MaxSize")
		asg.setAutoScalingMaxSize(maxSize + 1)
		defer asg.setAutoScalingMaxSize(maxSize)
	}

	log.Printf("Attaching spot instance %s to the group %s",
		*i.InstanceId, asg.name)
	err = asg.attachSpotInstance(*i.InstanceId, true)

	if err != nil {
		log.Printf("Spot instance %s couldn't be attached to the group %s, terminating it...",
			*i.InstanceId, asg.name)
		i.terminate()
		return nil, fmt.Errorf("couldn't attach spot instance %s ", *i.InstanceId)
	}

	log.Printf("Terminating on-demand instance %s from the group %s",
		*odInstance.InstanceId, asg.name)
	if err := asg.terminateInstanceInAutoScalingGroup(odInstance.Instance.InstanceId, true, true); err != nil {
		log.Printf("On-demand instance %s couldn't be terminated, re-trying...",
			*odInstance.InstanceId)
		return nil, fmt.Errorf("couldn't terminate on-demand instance %s",
			*odInstance.InstanceId)
	}

	return odInstance, nil
}

func (i *instance) getSwapCandidate() (*instance, error) {
	odInstanceID := i.getReplacementTargetInstanceID()
	if odInstanceID == nil {
		log.Println("Couldn't find target on-demand instance of", *i.InstanceId)
		return nil, fmt.Errorf("couldn't find target instance for %s", *i.InstanceId)
	}

	if err := i.region.scanInstance(odInstanceID); err != nil {
		log.Printf("Couldn't describe the target on-demand instance %s", *odInstanceID)
		return nil, fmt.Errorf("target instance %s couldn't be described", *odInstanceID)
	}

	odInstance := i.region.instances.get(*odInstanceID)
	if odInstance == nil {
		log.Printf("Target on-demand instance %s couldn't be found", *odInstanceID)
		return nil, fmt.Errorf("target instance %s is missing", *odInstanceID)
	}

	if !odInstance.shouldBeReplacedWithSpot() {
		log.Printf("Target on-demand instance %s shouldn't be replaced", *odInstanceID)
		i.terminate()
		return nil, fmt.Errorf("target instance %s should not be replaced with spot",
			*odInstanceID)
	}
	return odInstance, nil
}

func (i *instance) terminate() error {
	var err error
	log.Printf("Instance: %v\n", i)

	log.Printf("Terminating %v", *i.InstanceId)
	svc := i.region.services.ec2

	if !i.canTerminate() {
		log.Printf("Can't terminate %v, current state: %s",
			*i.InstanceId, *i.State.Name)
		return fmt.Errorf("can't terminate %s", *i.InstanceId)
	}

	_, err = svc.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{i.InstanceId},
	})

	if err != nil {
		log.Printf("Issue while terminating %v: %v", *i.InstanceId, err.Error())
	}

	return err
}

func (i *instance) deleteLaunchTemplate(ltName *string) {
	_, err := i.region.services.ec2.DeleteLaunchTemplate(&ec2.DeleteLaunchTemplateInput{
		LaunchTemplateName: ltName,
	})

	if err != nil {
		log.Printf("Issue while deleting launch template %v, error: %v", *ltName, err.Error())
	}
}
