package autospotting

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/davecgh/go-spew/spew"
)

// The key in this map is the instance ID, useful for quick retrieval of
// instance attributes.
type instanceMap map[string]*instance

type instanceManager struct {
	sync.RWMutex
	catalog instanceMap
}

type instances interface {
	add(inst *instance)
	get(string) *instance
	count() int
	count64() int64
	make()
	instances() <-chan *instance
	dump() string
}

func makeInstances() instances {
	return &instanceManager{catalog: instanceMap{}}
}

func makeInstancesWithCatalog(catalog instanceMap) instances {
	return &instanceManager{catalog: catalog}
}

func (is *instanceManager) dump() string {
	is.RLock()
	defer is.RUnlock()
	return spew.Sdump(is.catalog)
}
func (is *instanceManager) make() {
	is.Lock()
	is.catalog = make(instanceMap)
	is.Unlock()
}

func (is *instanceManager) add(inst *instance) {
	if inst == nil {
		return
	}

	is.Lock()
	defer is.Unlock()
	is.catalog[*inst.InstanceId] = inst
}

func (is *instanceManager) get(id string) (inst *instance) {
	is.RLock()
	defer is.RUnlock()
	return is.catalog[id]
}

func (is *instanceManager) count() int {
	is.RLock()
	defer is.RUnlock()

	return len(is.catalog)
}

func (is *instanceManager) count64() int64 {
	return int64(is.count())
}

func (is *instanceManager) instances() <-chan *instance {
	retC := make(chan *instance)
	go func() {
		is.RLock()
		defer is.RUnlock()
		defer close(retC)
		for _, i := range is.catalog {
			retC <- i
		}
	}()

	return retC
}

// instance wraps an ec2.instance and has some additional fields and functions
type instance struct {
	*ec2.Instance
	typeInfo  instanceTypeInformation
	price     float64
	region    *region
	protected bool
	asg       *autoScalingGroup
}

type acceptableInstance struct {
	instanceTI instanceTypeInformation
	price      float64
}

type instanceTypeInformation struct {
	instanceType             string
	vCPU                     int
	PhysicalProcessor        string
	GPU                      int
	pricing                  prices
	memory                   float32
	virtualizationTypes      []string
	hasInstanceStore         bool
	instanceStoreDeviceSize  float32
	instanceStoreDeviceCount int
	instanceStoreIsSSD       bool
	hasEBSOptimization       bool
	EBSThroughput            float32
}

func (i *instance) calculatePrice(spotCandidate instanceTypeInformation) float64 {
	spotPrice := spotCandidate.pricing.spot[*i.Placement.AvailabilityZone]
	debug.Println("Comparing price spot/instance:")

	if i.EbsOptimized != nil && *i.EbsOptimized {
		spotPrice += spotCandidate.pricing.ebsSurcharge
		debug.Println("\tEBS Surcharge : ", spotCandidate.pricing.ebsSurcharge)
	}

	debug.Println("\tSpot price: ", spotPrice)
	debug.Println("\tInstance price: ", i.price)
	return spotPrice
}

func (i *instance) isSpot() bool {
	return i.InstanceLifecycle != nil &&
		*i.InstanceLifecycle == "spot"
}

func (i *instance) isProtectedFromTermination() (bool, error) {

	debug.Println("\tCheching termination protection for instance: ", *i.InstanceId)
	// determine and set the API termination protection field
	diaRes, err := i.region.services.ec2.DescribeInstanceAttribute(
		&ec2.DescribeInstanceAttributeInput{
			Attribute:  aws.String("disableApiTermination"),
			InstanceId: i.InstanceId,
		})

	if err != nil {
		// better safe than sorry!
		logger.Printf("Couldn't describe instance attritbutes, assuming instance %v is protected: %v\n",
			*i.InstanceId, err.Error())
		return true, err
	}

	if diaRes != nil &&
		diaRes.DisableApiTermination != nil &&
		diaRes.DisableApiTermination.Value != nil &&
		*diaRes.DisableApiTermination.Value {
		logger.Printf("\t: %v Instance, %v is protected from termination\n",
			*i.Placement.AvailabilityZone, *i.InstanceId)
		return true, nil
	}
	return false, nil
}

func (i *instance) isProtectedFromScaleIn() bool {
	if i.asg == nil {
		return false
	}

	for _, inst := range i.asg.Instances {
		if *inst.InstanceId == *i.InstanceId &&
			*inst.ProtectedFromScaleIn {
			logger.Printf("\t: %v Instance, %v is protected from scale-in\n",
				*inst.AvailabilityZone,
				*inst.InstanceId)
			return true
		}
	}
	return false
}

func (i *instance) canTerminate() bool {
	return *i.State.Name != ec2.InstanceStateNameTerminated &&
		*i.State.Name != ec2.InstanceStateNameShuttingDown
}

func (i *instance) terminate() error {
	var err error
	logger.Printf("Instance: %v\n", i)

	logger.Printf("Terminating %v", *i.InstanceId)
	svc := i.region.services.ec2

	if !i.canTerminate() {
		logger.Printf("Can't terminate %v, current state: %s",
			*i.InstanceId, *i.State.Name)
		return fmt.Errorf("can't terminate %s", *i.InstanceId)
	}

	_, err = svc.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{i.InstanceId},
	})

	if err != nil {
		logger.Printf("Issue while terminating %v: %v", *i.InstanceId, err.Error())
	}

	return err
}

func (i *instance) shouldBeReplacedWithSpot() bool {
	protT, _ := i.isProtectedFromTermination()
	return i.belongsToEnabledASG() &&
		i.asgNeedsReplacement() &&
		!i.isSpot() &&
		!i.isProtectedFromScaleIn() &&
		!protT
}

func (i *instance) belongsToEnabledASG() bool {
	belongs, asgName := i.belongsToAnASG()
	if !belongs {
		logger.Printf("%s instane %s doesn't belong to any ASG",
			i.region.name, *i.InstanceId)
		return false
	}

	for _, asg := range i.region.enabledASGs {
		if asg.name == *asgName && asg.isEnabledForEventBasedInstanceReplacement() {
			asg.config = i.region.conf.AutoScalingConfig
			asg.scanInstances()
			asg.loadDefaultConfig()
			asg.loadConfigFromTags()
			asg.loadLaunchConfiguration()
			asg.populateASGInstancesInService()
			i.asg = &asg
			i.price = i.typeInfo.pricing.onDemand / i.region.conf.OnDemandPriceMultiplier * i.asg.config.OnDemandPriceMultiplier
			logger.Printf("%s instace %s belongs to enabled ASG %s", i.region.name,
				*i.InstanceId, i.asg.name)
			return true
		}
	}
	return false
}

func (i *instance) belongsToAnASG() (bool, *string) {
	for _, tag := range i.Tags {
		if *tag.Key == "aws:autoscaling:groupName" {
			return true, tag.Value
		}
	}
	return false, nil
}

func (i *instance) asgNeedsReplacement() bool {
	ret := i.asg.needReplaceOnDemandInstances()
	return ret
}

func (i *instance) isPriceCompatible(spotPrice float64) bool {
	if spotPrice == 0 {
		debug.Printf("\tUnavailable in this Availability Zone")
		return false
	}

	if spotPrice <= i.price {
		return true
	}

	debug.Printf("\tNot price compatible")
	return false
}

func (i *instance) isClassCompatible(spotCandidate instanceTypeInformation) bool {
	current := i.typeInfo

	debug.Println("Comparing class spot/instance:")
	debug.Println("\tSpot CPU/memory/GPU: ", spotCandidate.vCPU,
		" / ", spotCandidate.memory, " / ", spotCandidate.GPU)
	debug.Println("\tInstance CPU/memory/GPU: ", current.vCPU,
		" / ", current.memory, " / ", current.GPU)

	if i.isSameArch(spotCandidate) &&
		spotCandidate.vCPU >= current.vCPU &&
		spotCandidate.memory >= current.memory &&
		spotCandidate.GPU >= current.GPU {
		return true
	}
	debug.Println("\tNot class compatible (CPU/memory/GPU)")
	return false
}

func (i *instance) isSameArch(other instanceTypeInformation) bool {
	thisCPU := i.typeInfo.PhysicalProcessor
	otherCPU := other.PhysicalProcessor

	ret := (isIntelCompatible(thisCPU) && isIntelCompatible(otherCPU)) ||
		(isARM(thisCPU) && isARM(otherCPU))

	if !ret {
		debug.Println("\tInstance CPU architecture mismatch, current CPU architecture",
			thisCPU, "is incompatible with candidate CPU architecture", otherCPU)
	}
	return ret
}

func isIntelCompatible(cpuName string) bool {
	return isIntel(cpuName) || isAMD(cpuName)
}

func isIntel(cpuName string) bool {
	// t1.micro seems to be the only one to have this set to 'Variable'
	return strings.Contains(cpuName, "Intel") || strings.Contains(cpuName, "Variable")
}

func isAMD(cpuName string) bool {
	return strings.Contains(cpuName, "AMD")
}

func isARM(cpuName string) bool {
	// The ARM chips are so far all called "AWS Graviton Processor"
	return strings.Contains(cpuName, "AWS")
}

func (i *instance) isEBSCompatible(spotCandidate instanceTypeInformation) bool {
	if spotCandidate.EBSThroughput < i.typeInfo.EBSThroughput {
		debug.Println("\tEBS throughput insufficient:", spotCandidate.EBSThroughput, "<", i.typeInfo.EBSThroughput)
		return false
	}
	return true
}

// Here we check the storage compatibility, with the following evaluation
// criteria:
// - speed: don't accept spinning disks when we used to have SSDs
// - number of volumes: the new instance should have enough volumes to be
//   able to attach all the instance store device mappings defined on the
//   original instance
// - volume size: each of the volumes should be at least as big as the
//   original instance's volumes
func (i *instance) isStorageCompatible(spotCandidate instanceTypeInformation, attachedVolumes int) bool {
	existing := i.typeInfo

	debug.Println("Comparing storage spot/instance:")
	debug.Println("\tSpot volumes/size/ssd: ",
		spotCandidate.instanceStoreDeviceCount,
		spotCandidate.instanceStoreDeviceSize,
		spotCandidate.instanceStoreIsSSD)
	debug.Println("\tInstance volumes/size/ssd: ",
		attachedVolumes,
		existing.instanceStoreDeviceSize,
		existing.instanceStoreIsSSD)

	if attachedVolumes == 0 ||
		(spotCandidate.instanceStoreDeviceCount >= attachedVolumes &&
			spotCandidate.instanceStoreDeviceSize >= existing.instanceStoreDeviceSize &&
			(spotCandidate.instanceStoreIsSSD ||
				spotCandidate.instanceStoreIsSSD == existing.instanceStoreIsSSD)) {
		return true
	}
	debug.Println("\tNot storage compatible")
	return false
}

func (i *instance) isVirtualizationCompatible(spotVirtualizationTypes []string) bool {
	current := *i.VirtualizationType
	if len(spotVirtualizationTypes) == 0 {
		spotVirtualizationTypes = []string{"HVM"}
	}
	debug.Println("Comparing virtualization spot/instance:")
	debug.Println("\tSpot virtualization: ", spotVirtualizationTypes)
	debug.Println("\tInstance virtualization: ", current)

	for _, avt := range spotVirtualizationTypes {
		if (avt == "PV") && (current == "paravirtual") ||
			(avt == "HVM") && (current == "hvm") {
			return true
		}
	}
	debug.Println("\tNot virtualization compatible")
	return false
}

func (i *instance) isAllowed(instanceType string, allowedList []string, disallowedList []string) bool {
	debug.Println("Checking allowed/disallowed list")

	if len(allowedList) > 0 {
		for _, a := range allowedList {
			if match, _ := filepath.Match(a, instanceType); match {
				return true
			}
		}
		debug.Println("\tNot in the list of allowed instance types")
		return false
	} else if len(disallowedList) > 0 {
		for _, a := range disallowedList {
			// glob matching
			if match, _ := filepath.Match(a, instanceType); match {
				debug.Println("\tIn the list of disallowed instance types")
				return false
			}
		}
	}
	return true
}

func (i *instance) getCompatibleSpotInstanceTypesListSortedAscendingByPrice(allowedList []string,
	disallowedList []string) ([]instanceTypeInformation, error) {
	current := i.typeInfo
	var acceptableInstanceTypes []acceptableInstance

	// Count the ephemeral volumes attached to the original instance's block
	// device mappings, this number is used later when comparing with each
	// instance type.

	usedMappings := i.asg.launchConfiguration.countLaunchConfigEphemeralVolumes()
	attachedVolumesNumber := min(usedMappings, current.instanceStoreDeviceCount)

	// Iterate alphabetically by instance type
	keys := make([]string, 0)
	for k := range i.region.instanceTypeInformation {
		keys = append(keys, k)
	}

	if len(keys) == 0 {
		logger.Println("Missing instance type information for ", i.region.name)
	}

	sort.Strings(keys)

	// Find all compatible and not blocked instance types
	for _, k := range keys {
		candidate := i.region.instanceTypeInformation[k]

		candidatePrice := i.calculatePrice(candidate)
		debug.Println("Comparing current type", current.instanceType, "with price", i.price,
			"with candidate", candidate.instanceType, "with price", candidatePrice)

		if i.isAllowed(candidate.instanceType, allowedList, disallowedList) &&
			i.isPriceCompatible(candidatePrice) &&
			i.isEBSCompatible(candidate) &&
			i.isClassCompatible(candidate) &&
			i.isStorageCompatible(candidate, attachedVolumesNumber) &&
			i.isVirtualizationCompatible(candidate.virtualizationTypes) {
			acceptableInstanceTypes = append(acceptableInstanceTypes, acceptableInstance{candidate, candidatePrice})
			logger.Println("\tFound compatible instance type", candidate.instanceType, "added to launch candiates list")
		} else if candidate.instanceType != "" {
			debug.Println("Non compatible option found:", candidate.instanceType, "at", candidatePrice, " - discarding")
		}
	}

	if acceptableInstanceTypes != nil {
		sort.Slice(acceptableInstanceTypes, func(i, j int) bool {
			return acceptableInstanceTypes[i].price < acceptableInstanceTypes[j].price
		})
		debug.Println("List of cheapest compatible spot instances found, sorted ascending by price: ",
			acceptableInstanceTypes)
		var result []instanceTypeInformation
		for _, ai := range acceptableInstanceTypes {
			result = append(result, ai.instanceTI)
		}
		return result, nil
	}

	return nil, fmt.Errorf("No cheaper spot instance types could be found")
}

func (i *instance) launchSpotReplacement() (*string, error) {
	i.price = i.typeInfo.pricing.onDemand / i.region.conf.OnDemandPriceMultiplier * i.asg.config.OnDemandPriceMultiplier
	instanceTypes, err := i.getCompatibleSpotInstanceTypesListSortedAscendingByPrice(
		i.asg.getAllowedInstanceTypes(i),
		i.asg.getDisallowedInstanceTypes(i))

	if err != nil {
		logger.Println("Couldn't determine the cheapest compatible spot instance type")
		return nil, err
	}

	//Go through all compatible instances until one type launches or we are out of options.
	for _, instanceType := range instanceTypes {
		az := *i.Placement.AvailabilityZone
		bidPrice := i.getPricetoBid(i.price,
			instanceType.pricing.spot[az])

		runInstancesInput := i.createRunInstancesInput(instanceType.instanceType, bidPrice)
		logger.Println(az, i.asg.name, "Launching spot instance of type", instanceType.instanceType, "with bid price", bidPrice)
		logger.Println(az, i.asg.name)
		resp, err := i.region.services.ec2.RunInstances(runInstancesInput)

		if err != nil {
			if strings.Contains(err.Error(), "InsufficientInstanceCapacity") {
				logger.Println("Couldn't launch spot instance due to lack of capcity, trying next instance type:", err.Error())
			} else {
				logger.Println("Couldn't launch spot instance:", err.Error(), "trying next instance type")
				debug.Println(runInstancesInput)
			}
		} else {
			spotInst := resp.Instances[0]
			logger.Println(i.asg.name, "Successfully launched spot instance", *spotInst.InstanceId,
				"of type", *spotInst.InstanceType,
				"with bid price", bidPrice,
				"current spot price", instanceType.pricing.spot[az])

			debug.Println("RunInstances response:", spew.Sdump(resp))
			return spotInst.InstanceId, nil
		}
	}

	logger.Println(i.asg.name, "Exhausted all compatible instance types without launch success. Aborting.")
	return nil, errors.New("exhausted all compatible instance types")

}

func (i *instance) getPricetoBid(
	baseOnDemandPrice float64, currentSpotPrice float64) float64 {

	logger.Println("BiddingPolicy: ", i.region.conf.BiddingPolicy)

	if i.region.conf.BiddingPolicy == DefaultBiddingPolicy {
		logger.Println("Bidding base on demand price", baseOnDemandPrice)
		return baseOnDemandPrice
	}

	bufferPrice := math.Min(baseOnDemandPrice, currentSpotPrice*(1.0+i.region.conf.SpotPriceBufferPercentage/100.0))
	logger.Println("Bidding buffer-based price", bufferPrice)
	return bufferPrice
}

func (i *instance) convertBlockDeviceMappings(lc *launchConfiguration) []*ec2.BlockDeviceMapping {
	bds := []*ec2.BlockDeviceMapping{}
	if lc == nil || len(lc.BlockDeviceMappings) == 0 {
		debug.Println("Missing block device mappings")
		return bds
	}

	for _, lcBDM := range lc.BlockDeviceMappings {

		ec2BDM := &ec2.BlockDeviceMapping{
			DeviceName:  lcBDM.DeviceName,
			VirtualName: lcBDM.VirtualName,
		}

		if lcBDM.Ebs != nil {
			ec2BDM.Ebs = &ec2.EbsBlockDevice{
				DeleteOnTermination: lcBDM.Ebs.DeleteOnTermination,
				Encrypted:           lcBDM.Ebs.Encrypted,
				Iops:                lcBDM.Ebs.Iops,
				SnapshotId:          lcBDM.Ebs.SnapshotId,
				VolumeSize:          lcBDM.Ebs.VolumeSize,
				VolumeType:          lcBDM.Ebs.VolumeType,
			}
		}

		// handle the noDevice field directly by skipping the device if set to true
		if lcBDM.NoDevice != nil && *lcBDM.NoDevice {
			continue
		}
		bds = append(bds, ec2BDM)

	}
	return bds
}

func (i *instance) convertSecurityGroups() []*string {
	groupIDs := []*string{}
	for _, sg := range i.SecurityGroups {
		groupIDs = append(groupIDs, sg.GroupId)
	}
	return groupIDs
}

func (i *instance) launchTemplateHasNetworkInterfaces(id, ver *string) (bool, []*ec2.LaunchTemplateInstanceNetworkInterfaceSpecification) {
	res, err := i.region.services.ec2.DescribeLaunchTemplateVersions(
		&ec2.DescribeLaunchTemplateVersionsInput{
			Versions:         []*string{ver},
			LaunchTemplateId: id,
		},
	)

	if err != nil {
		logger.Println("Failed to describe launch template", *id, "version", *ver,
			"encountered error:", err.Error())
	}

	if err == nil && len(res.LaunchTemplateVersions) == 1 {
		lt := res.LaunchTemplateVersions[0]
		nis := lt.LaunchTemplateData.NetworkInterfaces
		if len(nis) > 0 {
			return true, nis
		}
	}
	return false, nil
}

func (i *instance) createRunInstancesInput(instanceType string, price float64) *ec2.RunInstancesInput {
	var retval ec2.RunInstancesInput

	// information we must (or can safely) copy/convert from the currently running
	// on-demand instance or we had to compute in order to place the spot bid
	retval = ec2.RunInstancesInput{

		EbsOptimized: i.EbsOptimized,

		InstanceMarketOptions: &ec2.InstanceMarketOptionsRequest{
			MarketType: aws.String("spot"),
			SpotOptions: &ec2.SpotMarketOptions{
				MaxPrice: aws.String(strconv.FormatFloat(price, 'g', 10, 64)),
			},
		},

		InstanceType: aws.String(instanceType),
		MaxCount:     aws.Int64(1),
		MinCount:     aws.Int64(1),

		Placement: i.Placement,

		SecurityGroupIds: i.convertSecurityGroups(),

		SubnetId:          i.SubnetId,
		TagSpecifications: i.generateTagsList(),
	}

	if i.asg.LaunchTemplate != nil {
		ver := i.asg.LaunchTemplate.Version
		id := i.asg.LaunchTemplate.LaunchTemplateId

		retval.LaunchTemplate = &ec2.LaunchTemplateSpecification{
			LaunchTemplateId: id,
			Version:          ver,
		}

		if having, nis := i.launchTemplateHasNetworkInterfaces(id, ver); having {
			for _, ni := range nis {
				retval.NetworkInterfaces = append(retval.NetworkInterfaces,
					&ec2.InstanceNetworkInterfaceSpecification{
						AssociatePublicIpAddress: ni.AssociatePublicIpAddress,
						SubnetId:                 i.SubnetId,
						DeviceIndex:              ni.DeviceIndex,
						Groups:                   i.convertSecurityGroups(),
					},
				)
			}
			retval.SubnetId, retval.SecurityGroupIds = nil, nil
		}
	}

	if i.asg.launchConfiguration != nil {
		lc := i.asg.launchConfiguration

		if lc.KeyName != nil && *lc.KeyName != "" {
			retval.KeyName = lc.KeyName
		}

		if lc.IamInstanceProfile != nil {
			if strings.HasPrefix(*lc.IamInstanceProfile, "arn:aws:iam:") {
				retval.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{
					Arn: lc.IamInstanceProfile,
				}
			} else {
				retval.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{
					Name: lc.IamInstanceProfile,
				}
			}
		}
		retval.ImageId = lc.ImageId

		if strings.ToLower(i.asg.config.PatchBeanstalkUserdata) == "true" {
			retval.UserData = getPatchedUserDataForBeanstalk(lc.UserData)
		} else {
			retval.UserData = lc.UserData
		}

		BDMs := i.convertBlockDeviceMappings(lc)

		if len(BDMs) > 0 {
			retval.BlockDeviceMappings = BDMs
		}

		if lc.InstanceMonitoring != nil {
			retval.Monitoring = &ec2.RunInstancesMonitoringEnabled{
				Enabled: lc.InstanceMonitoring.Enabled}
		}

		if lc.AssociatePublicIpAddress != nil || i.SubnetId != nil {
			// Instances are running in a VPC.
			retval.NetworkInterfaces = []*ec2.InstanceNetworkInterfaceSpecification{
				{
					AssociatePublicIpAddress: lc.AssociatePublicIpAddress,
					DeviceIndex:              aws.Int64(0),
					SubnetId:                 i.SubnetId,
					Groups:                   i.convertSecurityGroups(),
				},
			}
			retval.SubnetId, retval.SecurityGroupIds = nil, nil
		}
	}

	return &retval
}

func (i *instance) generateTagsList() []*ec2.TagSpecification {
	tags := ec2.TagSpecification{
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

	for _, tag := range i.Tags {
		if !strings.HasPrefix(*tag.Key, "aws:") &&
			*tag.Key != "launched-by-autospotting" &&
			*tag.Key != "launched-for-asg" &&
			*tag.Key != "launched-for-replacing-instance" &&
			*tag.Key != "LaunchTemplateID" &&
			*tag.Key != "LaunchTemplateVersion" &&
			*tag.Key != "LaunchConfiguationName" {
			tags.Tags = append(tags.Tags, tag)
		}
	}
	return []*ec2.TagSpecification{&tags}
}

func (i *instance) getReplacementTargetASGName() *string {
	for _, tag := range i.Tags {
		if *tag.Key == "launched-for-asg" {
			return tag.Value
		}
	}
	return nil
}

func (i *instance) getReplacementTargetInstanceID() *string {
	for _, tag := range i.Tags {
		if *tag.Key == "launched-for-replacing-instance" {
			return tag.Value
		}
	}
	return nil
}

func (i *instance) isUnattachedSpotInstanceLaunchedForAnEnabledASG() bool {
	asgName := i.getReplacementTargetASGName()
	if asgName == nil {
		logger.Printf("%s is missing the tag value for 'launched-for-asg'", *i.InstanceId)
		return false
	}
	asg := i.region.findEnabledASGByName(*asgName)

	if asg != nil &&
		asg.isEnabledForEventBasedInstanceReplacement() &&
		!asg.hasMemberInstance(i) &&
		i.isSpot() {
		logger.Println("Found unattached spot instance", *i.InstanceId)
		return true
	}
	return false
}

func (i *instance) swapWithGroupMember(asg *autoScalingGroup) (*instance, error) {
	odInstanceID := i.getReplacementTargetInstanceID()
	if odInstanceID == nil {
		logger.Println("Couldn't find target on-demand instance of", *i.InstanceId)
		return nil, fmt.Errorf("couldn't find target instance for %s", *i.InstanceId)
	}

	if err := i.region.scanInstance(odInstanceID); err != nil {
		logger.Printf("Couldn't describe the target on-demand instance %s", *odInstanceID)
		return nil, fmt.Errorf("target instance %s couldn't be described", *odInstanceID)
	}

	odInstance := i.region.instances.get(*odInstanceID)
	if odInstance == nil {
		logger.Printf("Target on-demand instance %s couldn't be found", *odInstanceID)
		return nil, fmt.Errorf("target instance %s is missing", *odInstanceID)
	}

	if !odInstance.shouldBeReplacedWithSpot() {
		logger.Printf("Target on-demand instance %s shouldn't be replaced", *odInstanceID)
		i.terminate()
		return nil, fmt.Errorf("target instance %s should not be replaced with spot",
			*odInstanceID)
	}

	// var waiter sync.WaitGroup
	// defer waiter.Wait()
	// go asg.temporarilySuspendTerminations(&waiter)
	asg.suspendResumeProcess(*i.InstanceId, "suspend")
	defer asg.suspendResumeProcess(*i.InstanceId, "resume")

	logger.Printf("Attaching spot instance %s to the group %s",
		*i.InstanceId, asg.name)
	increase, err := asg.attachSpotInstance(*i.InstanceId, true)
	if increase > 0 {
		defer asg.changeAutoScalingMaxSize(int64(-1*increase), *i.InstanceId)
	}

	if err != nil {
		logger.Printf("Spot instance %s couldn't be attached to the group %s, terminating it...",
			*i.InstanceId, asg.name)
		i.terminate()
		return nil, fmt.Errorf("couldn't attach spot instance %s ", *i.InstanceId)
	}

	logger.Printf("Terminating on-demand instance %s from the group %s",
		*odInstanceID, asg.name)
	if err := asg.terminateInstanceInAutoScalingGroup(odInstanceID, true, true); err != nil {
		logger.Printf("On-demand instance %s couldn't be terminated, re-trying...",
			*odInstanceID)
		return nil, fmt.Errorf("couldn't terminate on-demand instance %s",
			*odInstanceID)
	}

	// asg.resumeTerminationProcess()
	// waiter.Done()
	return odInstance, nil
}

// returns an instance ID as *string, set to nil if we need to wait for the next
// run in case there are no spot instances
func (i *instance) isReadyToAttach(asg *autoScalingGroup) bool {

	logger.Println("Considering ", *i.InstanceId, "for attaching to", asg.name)

	gracePeriod := *asg.HealthCheckGracePeriod

	instanceUpTime := time.Now().Unix() - i.LaunchTime.Unix()

	logger.Println("Instance uptime:", time.Duration(instanceUpTime)*time.Second)

	// Check if the spot instance is out of the grace period, so in that case we
	// can replace an on-demand instance with it
	if *i.State.Name == ec2.InstanceStateNameRunning &&
		instanceUpTime > gracePeriod {
		logger.Println("The spot instance", *i.InstanceId,
			" has passed grace period and is ready to attach to the group.")
		return true
	} else if *i.State.Name == ec2.InstanceStateNameRunning &&
		instanceUpTime < gracePeriod {
		logger.Println("The spot instance", *i.InstanceId,
			"is still in the grace period,",
			"waiting for it to be ready before we can attach it to the group...")
		return false
	} else if *i.State.Name == ec2.InstanceStateNamePending {
		logger.Println("The spot instance", *i.InstanceId,
			"is still pending,",
			"waiting for it to be running before we can attach it to the group...")
		return false
	}
	return false
}
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}
