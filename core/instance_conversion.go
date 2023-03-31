// Copyright (c) 2016-2022 Cristian Măgherușan-Stanciu
// Licensed under the Open Software License version 3.0

package autospotting

// instance_conversion.go contains functions that help cloning OnDemand instance configuration to new Spot instances.

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
)

var unsupportedIO2Regions = [...]string{
	"us-gov-west-1",
	"us-gov-east-1",
	"sa-east-1",
	"cn-north-1",
	"cn-northwest-1",
	"eu-south-1",
	"af-south-1",
	"eu-west-3",
	"ap-northeast-3",
}

func (i *instance) getPriceToBid(
	baseOnDemandPrice float64, currentSpotPrice float64, spotPremium float64) float64 {

	debug.Println("BiddingPolicy: ", i.region.conf.BiddingPolicy)

	if i.region.conf.BiddingPolicy == DefaultBiddingPolicy {
		log.Println("Bidding base on demand price", baseOnDemandPrice, "to replace instance", *i.InstanceId)
		return baseOnDemandPrice
	}

	bufferPrice := math.Min(baseOnDemandPrice, ((currentSpotPrice-spotPremium)*(1.0+i.region.conf.SpotPriceBufferPercentage/100.0))+spotPremium)
	log.Println("Bidding buffer-based price of", bufferPrice, "based on current spot price of", currentSpotPrice,
		"and buffer percentage of", i.region.conf.SpotPriceBufferPercentage, "to replace instance", i.InstanceId)
	return bufferPrice
}

func (i *instance) convertLaunchConfigurationBlockDeviceMappings(BDMs []*autoscaling.BlockDeviceMapping) []*ec2.LaunchTemplateBlockDeviceMappingRequest {

	bds := []*ec2.LaunchTemplateBlockDeviceMappingRequest{}
	if len(BDMs) == 0 {
		debug.Println("Missing LC block device mappings")
	}

	for _, BDM := range BDMs {

		ec2BDM := &ec2.LaunchTemplateBlockDeviceMappingRequest{
			DeviceName:  BDM.DeviceName,
			VirtualName: BDM.VirtualName,
		}

		if BDM.Ebs != nil {
			ec2BDM.Ebs = &ec2.LaunchTemplateEbsBlockDeviceRequest{
				DeleteOnTermination: BDM.Ebs.DeleteOnTermination,
				Encrypted:           BDM.Ebs.Encrypted,
				Iops:                BDM.Ebs.Iops,
				SnapshotId:          BDM.Ebs.SnapshotId,
				VolumeSize:          BDM.Ebs.VolumeSize,
				VolumeType:          convertLaunchConfigurationEBSVolumeType(BDM.Ebs, i.asg),
			}
		}

		// handle the noDevice field directly by skipping the device if set to true
		if BDM.NoDevice != nil && *BDM.NoDevice {
			continue
		}
		bds = append(bds, ec2BDM)
	}

	if len(bds) == 0 {
		return nil
	}
	return bds
}

func (i *instance) convertLaunchTemplateBlockDeviceMappings(BDMs []*ec2.LaunchTemplateBlockDeviceMapping) []*ec2.LaunchTemplateBlockDeviceMappingRequest {

	bds := []*ec2.LaunchTemplateBlockDeviceMappingRequest{}
	if len(BDMs) == 0 {
		log.Println("Missing LT block device mappings")
	}

	for _, BDM := range BDMs {

		ec2BDM := &ec2.LaunchTemplateBlockDeviceMappingRequest{
			DeviceName:  BDM.DeviceName,
			VirtualName: BDM.VirtualName,
		}

		if BDM.Ebs != nil {
			ec2BDM.Ebs = &ec2.LaunchTemplateEbsBlockDeviceRequest{
				DeleteOnTermination: BDM.Ebs.DeleteOnTermination,
				Encrypted:           BDM.Ebs.Encrypted,
				Iops:                BDM.Ebs.Iops,
				SnapshotId:          BDM.Ebs.SnapshotId,
				VolumeSize:          BDM.Ebs.VolumeSize,
				VolumeType:          convertLaunchTemplateEBSVolumeType(BDM.Ebs, i.asg),
			}
		}

		// handle the noDevice field directly by skipping the device if set to true, apparently NoDevice is here a string instead of a bool.
		if BDM.NoDevice != nil && *BDM.NoDevice == "true" {
			continue
		}
		bds = append(bds, ec2BDM)
	}

	if len(bds) == 0 {
		return nil
	}
	return bds
}

func (i *instance) convertImageBlockDeviceMappings(BDMs []*ec2.BlockDeviceMapping) []*ec2.LaunchTemplateBlockDeviceMappingRequest {

	bds := []*ec2.LaunchTemplateBlockDeviceMappingRequest{}
	if len(BDMs) == 0 {
		log.Println("Missing Image block device mappings")
	}

	for _, BDM := range BDMs {

		ec2BDM := &ec2.LaunchTemplateBlockDeviceMappingRequest{
			DeviceName:  BDM.DeviceName,
			VirtualName: BDM.VirtualName,
		}

		if BDM.Ebs != nil {
			ec2BDM.Ebs = &ec2.LaunchTemplateEbsBlockDeviceRequest{
				DeleteOnTermination: BDM.Ebs.DeleteOnTermination,
				Encrypted:           BDM.Ebs.Encrypted,
				Iops:                BDM.Ebs.Iops,
				SnapshotId:          BDM.Ebs.SnapshotId,
				VolumeSize:          BDM.Ebs.VolumeSize,
				VolumeType:          convertImageEBSVolumeType(BDM.Ebs, i.asg),
			}
		}

		// handle the noDevice field directly by skipping the device if set to true, apparently NoDevice is here a string instead of a bool.
		if BDM.NoDevice != nil && *BDM.NoDevice == "true" {
			continue
		}
		bds = append(bds, ec2BDM)
	}

	if len(bds) == 0 {
		return nil
	}
	return bds
}

func convertLaunchConfigurationEBSVolumeType(ebs *autoscaling.Ebs, a *autoScalingGroup) *string {
	// convert IO1 to IO2 in supported regions
	r := a.region.name
	asg := a.name

	if ebs.VolumeType == nil {
		log.Println(r, ": Empty EBS VolumeType while converting LC volume for ASG", asg)
		return nil
	}

	if *ebs.VolumeType == "io1" && supportedIO2region(r) {
		log.Println(r, ": Converting IO1 volume to IO2 for new instance launched for", asg)
		return aws.String("io2")
	}

	// convert GP2 to GP3 below the threshold where GP2 becomes more performant. The Threshold is configurable
	if *ebs.VolumeType == "gp2" && *ebs.VolumeSize <= a.config.GP2ConversionThreshold {
		log.Println(r, ": Converting GP2 EBS volume to GP3 for new instance launched for", asg)
		return aws.String("gp3")
	}
	log.Println(r, ": No EBS volume conversion could be done for", asg)
	return ebs.VolumeType
}

func convertLaunchTemplateEBSVolumeType(ebs *ec2.LaunchTemplateEbsBlockDevice, a *autoScalingGroup) *string {
	// convert IO1 to IO2 in supported regions
	r := a.region.name
	asg := a.name
	if *ebs.VolumeType == "io1" && supportedIO2region(r) {
		log.Println(r, ": Converting IO1 volume to IO2 for new instance launched for", asg)
		return aws.String("io2")
	}

	// convert GP2 to GP3 below the threshold where GP2 becomes more performant. The Threshold is configurable
	if *ebs.VolumeType == "gp2" && *ebs.VolumeSize <= a.config.GP2ConversionThreshold {
		log.Println(r, ": Converting GP2 EBS volume to GP3 for new instance launched for", asg)
		return aws.String("gp3")
	}
	log.Println(r, ": No EBS volume conversion could be done for", asg)
	return ebs.VolumeType
}

func convertImageEBSVolumeType(ebs *ec2.EbsBlockDevice, a *autoScalingGroup) *string {
	// convert IO1 to IO2 in supported regions
	r := a.region.name
	asg := a.name
	if *ebs.VolumeType == "io1" && supportedIO2region(r) {
		log.Println(r, ": Converting IO1 volume to IO2 for new instance launched for", asg)
		return aws.String("io2")
	}

	// convert GP2 to GP3 below the threshold where GP2 becomes more performant. The Threshold is configurable
	if *ebs.VolumeType == "gp2" && *ebs.VolumeSize <= a.config.GP2ConversionThreshold {
		log.Println(r, ": Converting GP2 EBS volume to GP3 for new instance launched for", asg)
		return aws.String("gp3")
	}
	log.Println(r, ": No EBS volume conversion could be done for", asg)
	return ebs.VolumeType
}

func supportedIO2region(region string) bool {
	for _, r := range unsupportedIO2Regions {
		if region == r {
			log.Println("IO2 EBS volumes are not available in", region)
			return false
		}
	}
	return true
}

func (i *instance) convertSecurityGroups() []*string {
	groupIDs := []*string{}
	for _, sg := range i.SecurityGroups {
		groupIDs = append(groupIDs, sg.GroupId)
	}
	return groupIDs
}

func (i *instance) getlaunchTemplate(id, ver *string) (*ec2.ResponseLaunchTemplateData, error) {
	res, err := i.region.services.ec2.DescribeLaunchTemplateVersions(
		&ec2.DescribeLaunchTemplateVersionsInput{
			Versions:         []*string{ver},
			LaunchTemplateId: id,
		},
	)

	if err != nil {
		log.Println("Failed to describe launch template", *id, "version", *ver,
			"encountered error:", err.Error())
		return nil, err
	}
	if len(res.LaunchTemplateVersions) == 1 {
		return res.LaunchTemplateVersions[0].LaunchTemplateData, nil
	}
	return nil, fmt.Errorf("missing launch template version information")
}

func (i *instance) processLaunchTemplate(retval *ec2.RequestLaunchTemplateData) error {
	ver := i.asg.LaunchTemplate.Version
	id := i.asg.LaunchTemplate.LaunchTemplateId

	ltData, err := i.getlaunchTemplate(id, ver)
	if err != nil {
		return err
	}

	// see https://docs.aws.amazon.com/sdk-for-go/api/service/ec2/#RequestLaunchTemplateData for the attributes we need to copy over to the new LT

	// currently omitted fields:
	// ElasticGpuSpecifications - not sure about the use case for this, but I'm open to add it later
	// ElasticInferenceAccelerators - not sure about the use case for this, but I'm open to add it later
	// EnclaveOptions - not sure about the use case for this, but I'm open to add it later
	// HibernationOptions - not sure about the use case for this, but I'm open to add it later
	// InstanceMarketOptions - needs to be set to Spot anyway
	// InstanceType - not needed because we pass more instance types
	// KernelId - probably not needed, should be determined from the AMI
	// LicenseSpecifications - probably not needed, should be determined from the AMI
	// MetadataOptions - not sure what's the use case for changing this
	// Placement - determined dynamically when launching each Spot instance
	// RamDiskId probably not needed, should be determined from the AMI

	retval.BlockDeviceMappings = i.convertLaunchTemplateBlockDeviceMappings(ltData.BlockDeviceMappings)

	if ltData.CapacityReservationSpecification != nil {
		retval.CapacityReservationSpecification = &ec2.LaunchTemplateCapacityReservationSpecificationRequest{
			CapacityReservationPreference: ltData.CapacityReservationSpecification.CapacityReservationPreference,
			CapacityReservationTarget:     (*ec2.CapacityReservationTarget)(ltData.CapacityReservationSpecification.CapacityReservationTarget),
		}
	}

	retval.CpuOptions = (*ec2.LaunchTemplateCpuOptionsRequest)(ltData.CpuOptions)

	retval.CreditSpecification = (*ec2.CreditSpecificationRequest)(ltData.CreditSpecification)

	retval.DisableApiTermination = ltData.DisableApiTermination

	retval.EbsOptimized = ltData.EbsOptimized

	retval.IamInstanceProfile = (*ec2.LaunchTemplateIamInstanceProfileSpecificationRequest)(ltData.IamInstanceProfile)

	retval.ImageId = ltData.ImageId

	retval.InstanceInitiatedShutdownBehavior = ltData.InstanceInitiatedShutdownBehavior

	retval.KeyName = ltData.KeyName

	retval.Monitoring = (*ec2.LaunchTemplatesMonitoringRequest)(ltData.Monitoring)

	if having, nis := i.launchTemplateHasNetworkInterfaces(ltData); having {
		for _, ni := range nis {
			retval.NetworkInterfaces = append(retval.NetworkInterfaces,
				&ec2.LaunchTemplateInstanceNetworkInterfaceSpecificationRequest{
					AssociatePublicIpAddress: ni.AssociatePublicIpAddress,
					SubnetId:                 i.SubnetId,
					DeviceIndex:              ni.DeviceIndex,
					Groups:                   i.convertSecurityGroups(),
				},
			)
		}
		retval.SecurityGroupIds = nil
		retval.SecurityGroups = nil
	} else {
		retval.NetworkInterfaces = nil
		retval.SecurityGroupIds = append(retval.SecurityGroupIds, ltData.SecurityGroupIds...)
		retval.SecurityGroups = append(retval.SecurityGroups, ltData.SecurityGroups...)
	}

	if i.asg.config.PatchBeanstalkUserdata {
		retval.UserData = getPatchedUserDataForBeanstalk(ltData.UserData)
	} else {
		retval.UserData = ltData.UserData
	}

	// MELLO
	retval.TagSpecifications = []*ec2.LaunchTemplateTagSpecificationRequest{}
	for _, ts := range ltData.TagSpecifications {
		retval.TagSpecifications = append(retval.TagSpecifications,
			&ec2.LaunchTemplateTagSpecificationRequest{
				ResourceType: ts.ResourceType,
				Tags: ts.Tags,
			},
		)
	}

	return nil
}

func (i *instance) processLaunchConfiguration(retval *ec2.RequestLaunchTemplateData) {
	lc := i.asg.launchConfiguration

	if lc.KeyName != nil && *lc.KeyName != "" {
		retval.KeyName = lc.KeyName
	}

	if lc.IamInstanceProfile != nil {
		if strings.HasPrefix(*lc.IamInstanceProfile, "arn:aws:iam:") {
			retval.IamInstanceProfile = &ec2.LaunchTemplateIamInstanceProfileSpecificationRequest{
				Arn: lc.IamInstanceProfile,
			}
		} else {
			retval.IamInstanceProfile = &ec2.LaunchTemplateIamInstanceProfileSpecificationRequest{
				Name: lc.IamInstanceProfile,
			}
		}
	}
	retval.ImageId = lc.ImageId

	if i.asg.config.PatchBeanstalkUserdata {
		retval.UserData = getPatchedUserDataForBeanstalk(lc.UserData)
	} else {
		retval.UserData = lc.UserData
	}

	BDMs := i.convertLaunchConfigurationBlockDeviceMappings(lc.BlockDeviceMappings)

	if len(BDMs) > 0 {
		retval.BlockDeviceMappings = BDMs
	}

	if lc.InstanceMonitoring != nil {
		retval.Monitoring = &ec2.LaunchTemplatesMonitoringRequest{
			Enabled: lc.InstanceMonitoring.Enabled}
	}

	if lc.AssociatePublicIpAddress != nil || i.SubnetId != nil {
		// Instances are running in a VPC.
		retval.NetworkInterfaces = []*ec2.LaunchTemplateInstanceNetworkInterfaceSpecificationRequest{
			{
				AssociatePublicIpAddress: lc.AssociatePublicIpAddress,
				DeviceIndex:              aws.Int64(0),
				SubnetId:                 i.SubnetId,
				Groups:                   i.convertSecurityGroups(),
			},
		}
		retval.SecurityGroupIds = nil
	}
}

func (i *instance) processImageBlockDevices(rii *ec2.RequestLaunchTemplateData) {
	svc := i.region.services.ec2

	resp, err := svc.DescribeImages(
		&ec2.DescribeImagesInput{
			ImageIds: []*string{i.ImageId},
		})

	if err != nil {
		log.Println(err.Error())
		return
	}
	if len(resp.Images) == 0 {
		log.Println("missing image data")
		return
	}

	rii.BlockDeviceMappings = i.convertImageBlockDeviceMappings(resp.Images[0].BlockDeviceMappings)
}

func (i *instance) createLaunchTemplateData() (*ec2.RequestLaunchTemplateData, error) {

	placement := ec2.LaunchTemplatePlacementRequest(*i.Placement)

	ltData := ec2.RequestLaunchTemplateData{}

	// populate the base of the ltData fields from launch Template and launch
	// Configuration then set additional fields from computed values. SGs need to
	// be set first because they may also be set in the network configuration of
	// the LT or LC, in which case the below initialization will be reverted.

	ltData.SecurityGroupIds = i.convertSecurityGroups()

	i.processImageBlockDevices(&ltData)

	if i.asg.LaunchTemplate != nil {
		err := i.processLaunchTemplate(&ltData)
		if err != nil {
			log.Println("failed to process launch template, the resulting instance configuration may be incomplete", err.Error())
			return nil, err
		}
	}
	if i.asg.launchConfiguration != nil {
		i.processLaunchConfiguration(&ltData)
	}

	ltData.EbsOptimized = i.EbsOptimized

	ltData.InstanceMarketOptions = &ec2.LaunchTemplateInstanceMarketOptionsRequest{
		MarketType: aws.String(Spot),
		SpotOptions: &ec2.LaunchTemplateSpotMarketOptionsRequest{
			MaxPrice: aws.String(strconv.FormatFloat(i.price, 'g', 10, 64)),
		},
	}

	ltData.Placement = &placement

	//MELLO
	generatedTagSpecifications := i.generateTagsList()
	for _, ts := range ltData.TagSpecifications {
		if *ts.ResourceType != "instance" {
			generatedTagSpecifications = append(generatedTagSpecifications, ts)
		}
	}
	ltData.TagSpecifications = generatedTagSpecifications

	debug.Printf("ltData: %+#v\n", ltData)

	return &ltData, nil
}

func (i *instance) createFleetLaunchTemplate(ltData *ec2.RequestLaunchTemplateData) (*string, error) {
	ltName := "AutoSpotting-Temporary-LaunchTemplate-for-" + *i.Instance.InstanceId

	_, err := i.region.services.ec2.CreateLaunchTemplate(&ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(ltName),
		LaunchTemplateData: ltData,
	})

	if err != nil {
		log.Println("failed to create LaunchTemplate,", err.Error())
		// if the LT already exists maybe from a previous failed run we take it and use it
		if !strings.Contains(err.Error(), "AlreadyExistsException") {
			return nil, err
		}
		log.Println("Reusing existing LaunchTemplate ", ltName)
		err = nil
	}

	return &ltName, err
}

func (i *instance) createFleetInput(ltName *string, instanceTypes []*string) *ec2.CreateFleetInput {

	var overrides []*ec2.FleetLaunchTemplateOverridesRequest

	debug.Printf("instance Details: %+#v\n", i)

	for p, inst := range instanceTypes {
		override := ec2.FleetLaunchTemplateOverridesRequest{
			InstanceType: inst,
			SubnetId:     i.SubnetId,
		}
		if i.asg.config.SpotAllocationStrategy == "capacity-optimized-prioritized" {
			override.Priority = aws.Float64(float64(p))
		}
		overrides = append(overrides, &override)
	}

	retval := &ec2.CreateFleetInput{
		LaunchTemplateConfigs: []*ec2.FleetLaunchTemplateConfigRequest{
			{
				LaunchTemplateSpecification: &ec2.FleetLaunchTemplateSpecificationRequest{
					LaunchTemplateName: ltName,
					Version:            aws.String("$Latest"),
				},
				Overrides: overrides,
			},
		},
		SpotOptions: &ec2.SpotOptionsRequest{
			AllocationStrategy: aws.String(i.asg.config.SpotAllocationStrategy),
		},
		Type: aws.String("instant"),
		TargetCapacitySpecification: &ec2.TargetCapacitySpecificationRequest{
			SpotTargetCapacity:        aws.Int64(1),
			TotalTargetCapacity:       aws.Int64(1),
			DefaultTargetCapacityType: aws.String("spot"),
		},
	}
	return retval
}

func (i *instance) generateTagsList() []*ec2.LaunchTemplateTagSpecificationRequest {
	tags := ec2.LaunchTemplateTagSpecificationRequest{
		ResourceType: aws.String("instance"),
		Tags: []*ec2.Tag{
			{
				Key:   aws.String("launched-by-autospotting"),
				Value: aws.String("true"),
			},
			{
				Key:   aws.String("launched-for-asg"),
				Value: aws.String(i.asg.name),
			},
			{
				Key:   aws.String("launched-for-replacing-instance"),
				Value: i.InstanceId,
			},
		},
	}

	if i.asg.LaunchTemplate != nil {
		tags.Tags = append(tags.Tags, &ec2.Tag{
			Key:   aws.String("LaunchTemplateID"),
			Value: i.asg.LaunchTemplate.LaunchTemplateId,
		})
		tags.Tags = append(tags.Tags, &ec2.Tag{
			Key:   aws.String("LaunchTemplateVersion"),
			Value: i.asg.LaunchTemplate.Version,
		})
	} else if i.asg.LaunchConfigurationName != nil {
		tags.Tags = append(tags.Tags, &ec2.Tag{
			Key:   aws.String("LaunchConfigurationName"),
			Value: i.asg.LaunchConfigurationName,
		})
	}

	tags.Tags = append(tags.Tags, filterTags(i.Tags)...)

	return []*ec2.LaunchTemplateTagSpecificationRequest{&tags}
}

func filterTags(tags []*ec2.Tag) []*ec2.Tag {
	var tl []*ec2.Tag

	var tagsToSkip = []string{
		"launched-by-autospotting",
		"launched-for-asg",
		"launched-for-replacing-instance",
		"LaunchTemplateID",
		"LaunchTemplateVersion",
		"LaunchConfigurationName",
	}

	for _, tag := range tags {
		if !strings.HasPrefix(*tag.Key, "aws:") &&
			!itemInSlice(*tag.Key, tagsToSkip) {
			tl = append(tl, tag)
		}
	}
	return tl
}
