// Copyright (c) 2016-2022 Cristian Măgherușan-Stanciu
// Licensed under the Open Software License version 3.0

package autospotting

import (
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws/endpoints"
	ec2instancesinfo "github.com/mello7tre/ec2-instances-info"
	"github.com/namsral/flag"
)

const (
	// AutoScalingTerminationMethod uses the TerminateInstanceInAutoScalingGroup
	// API method to terminate instances.  This method is recommended because it
	// will require termination Lifecycle Hooks that have been configured on the
	// Auto Scaling Group to be invoked before terminating the instance.  It's
	// also safe even if there are no such hooks configured.
	AutoScalingTerminationMethod = "autoscaling"

	// DetachTerminationMethod detaches the instance from the Auto Scaling Group
	// and then terminates it.  This method exists for historical reasons and is
	// no longer recommended.
	DetachTerminationMethod = "detach"

	// TerminateTerminationNotificationAction terminate the spot instance, which will be terminated
	// by AWS in 2 minutes, without reducing the ASG capacity, so that a new instance will
	// be launched. LifeCycle Hooks are triggered.
	TerminateTerminationNotificationAction = "terminate"

	// DetachTerminationNotificationAction detach the spot instance, which will be terminated
	// by AWS in 2 minutes, without reducing the ASG capacity, so that a new instance will
	// be launched. LifeCycle Hooks are not triggered.
	DetachTerminationNotificationAction = "detach"

	// AutoTerminationNotificationAction if ASG has a LifeCycleHook with LifecycleTransition = EC2_INSTANCE_TERMINATING
	// terminate the spot instance (as TerminateTerminationNotificationAction), if not detach it.
	AutoTerminationNotificationAction = "auto"

	// DefaultCronSchedule is the default value for the execution schedule in
	// simplified Cron-style definition the cron format only accepts the hour and
	// day of week fields, for example "9-18 1-5" would define the working week
	// hours. AutoSpotting will only run inside this time interval. The action can
	// also be reverted using the CronScheduleState parameter, so in order to run
	// outside this interval set the CronScheduleState to "off" either globally or
	// on a per-group override.
	DefaultCronSchedule = "* *"

	// Spot stores the string "spot"  to avoid typos as it's used in various places
	Spot = "spot"
	// OnDemand  stores the string "on-demand" to avoid typos as it's used in various places
	OnDemand = "on-demand"
	// DefaultGP2ConversionThreshold is the size under which GP3 is more performant than GP2 for both throughput and IOPS
	DefaultGP2ConversionThreshold = 170
)

// Config extends the AutoScalingConfig struct and in addition contains a
// number of global flags.
type Config struct {
	AutoScalingConfig

	// Static data fetched from ec2instances.info
	InstanceData *ec2instancesinfo.InstanceData

	// Logging
	LogFile io.Writer
	LogFlag int

	// The regions where it should be running, given as a single CSV-string
	Regions string

	// The region where the Lambda function is deployed
	MainRegion string

	// This is only here for tests, where we want to be able to somehow mock
	// time.Sleep without actually sleeping. While testing it defaults to 0 (which won't sleep at all), in
	// real-world usage it's expected to be set to 1
	SleepMultiplier time.Duration

	// Filter on ASG tags
	// for example: spot-enabled=true,environment=dev,team=interactive
	FilterByTags string
	// Controls how are the tags used to filter the groups.
	// Available options: 'opt-in' and 'opt-out', default: 'opt-in'
	TagFilteringMode string

	// The AutoSpotting version
	Version string

	// The percentage of the savings
	SavingsCut float64

	// The license of this AutoSpotting build - obsolete
	LicenseType string

	// Controls whether AutoSpotting patches Elastic Beanstalk UserData scripts to use
	// the instance role when calling CloudFormation helpers instead of the standard CloudFormation
	// authentication method
	PatchBeanstalkUserdata bool

	// JSON file containing event data used for locally simulating execution from Lambda.
	EventFile string

	// Final Recap String Array to show actions taken by ScheduleRun on ASGs
	FinalRecap map[string][]string

	// SQS Queue URl
	SQSQueueURL string

	// SQS MessageID
	sqsReceiptHandle string

	// DisableEventBasedInstanceReplacement forces execution in cron mode only
	DisableEventBasedInstanceReplacement bool

	// DisableInstanceRebalanceRecommendation disable the handling of Instance Rebalance Recommendation events.
	DisableInstanceRebalanceRecommendation bool

	// BillingOnly - only billing related actions will be taken, no instance replacement will be performed.
	BillingOnly bool
}

// ParseConfig loads configuration from command line flags, environments variables, and config files.
func ParseConfig(conf *Config) {

	// The use of FlagSet allows us to parse config multiple times, which is useful for unit tests.
	flagSet := flag.NewFlagSet("AutoSpotting", flag.ExitOnError)

	var region string

	if r := os.Getenv("AWS_REGION"); r != "" {
		region = r
	} else {
		region = endpoints.UsEast1RegionID
	}

	conf.LogFile = os.Stdout
	conf.LogFlag = log.Ldate | log.Ltime | log.Lshortfile

	log.SetOutput(conf.LogFile)
	log.SetFlags(conf.LogFlag)

	conf.MainRegion = region
	conf.SleepMultiplier = 1
	conf.sqsReceiptHandle = ""

	flagSet.StringVar(&conf.AllowedInstanceTypes, "allowed_instance_types", "",
		"\n\tIf specified, the spot instances will be searched only among these types.\n\tIf missing, any instance type is allowed.\n"+
			"\tAccepts a list of comma or whitespace separated instance types (supports globs).\n"+
			"\tExample: ./AutoSpotting -allowed_instance_types 'c5.*,c4.xlarge'\n")

	flagSet.StringVar(&conf.BiddingPolicy, "bidding_policy", DefaultBiddingPolicy,
		"\n\tPolicy choice for spot bid. If set to 'normal', we bid at the on-demand price(times the multiplier).\n"+
			"\tIf set to 'aggressive', we bid at a percentage value above the spot price \n"+
			"\tconfigurable using the spot_price_buffer_percentage.\n")

	flagSet.StringVar(&conf.DisallowedInstanceTypes, "disallowed_instance_types", "",
		"\n\tIf specified, the spot instances will _never_ be of these types.\n"+
			"\tAccepts a list of comma or whitespace separated instance types (supports globs).\n"+
			"\tExample: ./AutoSpotting -disallowed_instance_types 't2.*,c4.xlarge'\n")

	flagSet.StringVar(&conf.InstanceTerminationMethod, "instance_termination_method", DefaultInstanceTerminationMethod,
		"\n\tInstance termination method.  Must be one of '"+DefaultInstanceTerminationMethod+"' (default),\n"+
			"\t or 'detach' (compatibility mode, not recommended)\n")

	flagSet.StringVar(&conf.TerminationNotificationAction, "termination_notification_action", DefaultTerminationNotificationAction,
		"\n\tTermination Notification Action.\n"+
			"\tValid choices:\n"+
			"\t'"+DefaultTerminationNotificationAction+
			"' (terminate if lifecyclehook else detach) | 'terminate' (lifecyclehook triggered)"+
			" | 'detach' (lifecyclehook not triggered)\n")

	flagSet.Int64Var(&conf.MinOnDemandNumber, "min_on_demand_number", DefaultMinOnDemandValue,
		"\n\tNumber of on-demand nodes to be kept running in each of the groups.\n\t"+
			"Can be overridden on a per-group basis using the tag "+OnDemandNumberLong+".\n")

	flagSet.Float64Var(&conf.MinOnDemandPercentage, "min_on_demand_percentage", 0.0,
		"\n\tPercentage of the total number of instances in each group to be kept on-demand\n\t"+
			"Can be overridden on a per-group basis using the tag "+OnDemandPercentageTag+
			"\n\tIt is ignored if min_on_demand_number is also set.\n")

	flagSet.Float64Var(&conf.OnDemandPriceMultiplier, "on_demand_price_multiplier", DefaultOnDemandPriceMultiplier,
		"\n\tMultiplier for the on-demand price. Numbers less than 1.0 are useful for volume discounts.\n"+
			"The tag "+OnDemandPriceMultiplierTag+" can be used to override this on a group level.\n"+
			"\tExample: ./AutoSpotting -on_demand_price_multiplier 0.6 will have the on-demand price "+
			"considered at 60% of the actual value.\n")

	flagSet.StringVar(&conf.Regions, "regions", "",
		"\n\tRegions where it should be activated (separated by comma or whitespace, also supports globs).\n"+
			"\tBy default it runs on all regions.\n"+
			"\tExample: ./AutoSpotting -regions 'eu-*,us-east-1'\n")

	flagSet.Float64Var(&conf.SpotPriceBufferPercentage, "spot_price_buffer_percentage", DefaultSpotPriceBufferPercentage,
		"\n\tBid a given percentage above the current spot price.\n\tProtects the group from running spot"+
			"instances that got significantly more expensive than when they were initially launched\n"+
			"\tThe tag "+SpotPriceBufferPercentageTag+" can be used to override this on a group level.\n"+
			"\tIf the bid exceeds the on-demand price, we place a bid at on-demand price itself.\n")

	flagSet.StringVar(&conf.SpotProductDescription, "spot_product_description", DefaultSpotProductDescription,
		"\n\tThe Spot Product to use when looking up spot price history in the market.\n"+
			"\tValid choices: Linux/UNIX | SUSE Linux | Windows | Linux/UNIX (Amazon VPC) | \n"+
			"\tSUSE Linux (Amazon VPC) | Windows (Amazon VPC) | Red Hat Enterprise Linux\n\tDefault value: "+DefaultSpotProductDescription+"\n")

	flagSet.Float64Var(&conf.SpotProductPremium, "spot_product_premium", DefaultSpotProductPremium,
		"\n\tThe Product Premium to apply to the on demand price to improve spot selection and savings calculations\n"+
			"\twhen using a premium instance type such as RHEL.")

	flagSet.StringVar(&conf.TagFilteringMode, "tag_filtering_mode", "opt-in", "\n\tControls the behavior of the tag_filters option.\n"+
		"\tValid choices: opt-in | opt-out\n\tDefault value: 'opt-in'\n\tExample: ./AutoSpotting --tag_filtering_mode opt-out\n")

	flagSet.StringVar(&conf.FilterByTags, "tag_filters", "", "\n\tSet of tags to filter the ASGs on.\n"+
		"\tDefault if no value is set will be the equivalent of -tag_filters 'spot-enabled=true'\n"+
		"\tIn case the tag_filtering_mode is set to opt-out, it defaults to 'spot-enabled=false'\n"+
		"\tExample: ./AutoSpotting --tag_filters 'spot-enabled=true,Environment=dev,Team=vision'\n")

	flagSet.StringVar(&conf.CronSchedule, "cron_schedule", DefaultCronSchedule, "\n\tCron-like schedule in which to"+
		"\tperform(or not) spot replacement actions. Format: hour day-of-week\n"+
		"\tExample: ./AutoSpotting --cron_schedule '9-18 1-5' # workdays during the office hours \n")

	flagSet.StringVar(&conf.CronTimezone, "cron_timezone", "UTC", "\n\tTimezone to"+
		"\tperform(or not) spot replacement actions. Format: timezone\n"+
		"\tExample: ./AutoSpotting --cron_timezone 'Europe/London' \n")

	flagSet.StringVar(&conf.CronScheduleState, "cron_schedule_state", "on", "\n\tControls whether to take actions "+
		"inside or outside the schedule defined by cron_schedule. Allowed values: on|off\n"+
		"\tExample: ./AutoSpotting --cron_schedule_state='off' --cron_schedule '9-18 1-5'  # would only take action outside the defined schedule\n")

	flagSet.StringVar(&conf.LicenseType, "license", "evaluation", "\n\t - obsoleted, kept for compatibility only\n"+
		"\tExample: ./AutoSpotting --license evaluation\n")

	flagSet.StringVar(&conf.EventFile, "event_file", "", "\n\tJSON file containing event data, "+
		"used for locally simulating execution from Lambda. AutoSpotting now expects to be "+
		"triggered by events and won't do anything if no event is passed either as result of "+
		"AWS instance state notifications or simulated manually using this flag.\n")

	flagSet.StringVar(&conf.SQSQueueURL, "sqs_queue_url", "", "\n\tThe Url of the SQS fifo queue used to manage spot replacement actions. "+
		"This needs to exist in the same region as the main AutoSpotting Lambda function"+
		"\tExample: ./AutoSpotting --sqs_queue_url https://sqs.{AwsRegion}.amazonaws.com/{AccountId}/AutoSpotting.fifo\n")

	flagSet.BoolVar(&conf.PatchBeanstalkUserdata, "patch_beanstalk_userdata", false,
		"\n\tControls whether AutoSpotting patches Elastic Beanstalk UserData scripts to use the "+
			"instance role when calling CloudFormation helpers instead of the standard CloudFormation "+
			"authentication method\n"+
			"\tExample: ./AutoSpotting --patch_beanstalk_userdata true\n")

	flagSet.Int64Var(&conf.GP2ConversionThreshold, "ebs_gp2_conversion_threshold", DefaultGP2ConversionThreshold,
		"\n\tThe EBS volume size below which to automatically replace GP2 EBS volumes to the newer GP3 "+
			"volume type, that's 20% cheaper and more performant than GP2 for smaller sizes, but it's not "+
			"getting more performant wth size as GP2 does. Over 170 GB GP2 gets better throughput, and at "+
			"1TB GP2 also has better IOPS than a baseline GP3 volume.\n"+
			"\tExample: ./AutoSpotting --ebs_gp2_conversion_threshold 170\n")

	flagSet.BoolVar(&conf.DisableEventBasedInstanceReplacement, "disable_event_based_instance_replacement", false,
		"\n\tDisables the event based instance replacement, forcing the legacy cron mode.\n"+
			"\tExample: ./AutoSpotting --disable_event_based_instance_replacement=true\n")

	flagSet.BoolVar(&conf.DisableInstanceRebalanceRecommendation, "disable_instance_rebalance_recommendation", false,
		"\n\tDisables handling of instance rebalance recommendation events.\n"+
			"\tExample: ./AutoSpotting --disable_instance_rebalance_recommendation=true\n")

	flagSet.StringVar(&conf.SpotAllocationStrategy, "spot_allocation_strategy", "capacity-optimized-prioritized",
		"\n\tControls the Spot allocation strategy for launching Spot instances. Allowed options: \n"+
			"\t'capacity-optimized-prioritized' (default), 'capacity-optimized', 'lowest-price'.\n"+
			"\tFurther information on this is available at "+
			"https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-fleet-allocation-strategy.html\n"+
			"\tExample: ./AutoSpotting --spot_allocation_strategy capacity-optimized-prioritized\n")

	flagSet.BoolVar(&conf.BillingOnly, "billing_only", false,
		"\n\tControls whether AutoSpotting only does the Marketplace billing without taking any further\n"+
			"replacement actions when executed in cron mode\n"+
			"\tExample: ./AutoSpotting --billing_only true\n")

	printVersion := flagSet.Bool("version", false, "Print version number and exit.\n")

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		fmt.Printf("Error parsing config: %s\n", err.Error())
	}

	if *printVersion {
		fmt.Println("AutoSpotting build:", conf.Version)
		os.Exit(0)
	}

	data, err := ec2instancesinfo.Data()
	if err != nil {
		log.Fatal(err.Error())
	}
	conf.InstanceData = data

	conf.FinalRecap = make(map[string][]string)
}
