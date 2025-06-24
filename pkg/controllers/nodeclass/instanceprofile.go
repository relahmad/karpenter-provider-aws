/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodeclass

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	sdk "github.com/aws/karpenter-provider-aws/pkg/aws"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
	"github.com/aws/karpenter-provider-aws/pkg/providers/instanceprofile"
)

type InstanceProfile struct {
	instanceProfileProvider instanceprofile.Provider
	region                  string
	ec2api                  sdk.EC2API
	provisioner             *provisioning.Provisioner
}

func NewInstanceProfileReconciler(instanceProfileProvider instanceprofile.Provider, region string, ec2api sdk.EC2API) *InstanceProfile {
	return &InstanceProfile{
		instanceProfileProvider: instanceProfileProvider,
		region:                  region,
		ec2api:                  ec2api,
	}
}

func (ip *InstanceProfile) Reconcile(ctx context.Context, nodeClass *v1.EC2NodeClass) (reconcile.Result, error) {
	if nodeClass.Spec.Role != "" {
		// Initialize status if needed
		if nodeClass.Status.InstanceProfiles == nil {
			nodeClass.Status.InstanceProfiles = &v1.InstanceProfilesStatus{
				Current:  "",
				Previous: []string{},
				Version:  0,
			}
		}
		var currentRole string
		if nodeClass.Status.InstanceProfiles.Current != "" {
			if profile, err := ip.instanceProfileProvider.Get(ctx, nodeClass.Status.InstanceProfiles.Current); err == nil {
				if len(profile.Roles) > 0 {
					currentRole = lo.FromPtr(profile.Roles[0].RoleName)
				}
			}
		}

		if currentRole != nodeClass.Spec.Role {
			nodeClass.Status.InstanceProfiles.Version++
			profileName := nodeClass.InstanceProfileName(options.FromContext(ctx).ClusterName, ip.region)
			if err := ip.instanceProfileProvider.Create(
				ctx,
				profileName,
				nodeClass.InstanceProfileRole(),
				nodeClass.InstanceProfileTags(options.FromContext(ctx).ClusterName, ip.region),
			); err != nil {
				return reconcile.Result{}, fmt.Errorf("creating instance profile, %w", err)
			}
			nodeClass.Status.InstanceProfile = profileName

			if nodeClass.Status.InstanceProfiles.Current != "" {
				nodeClass.Status.InstanceProfiles.Previous = append(nodeClass.Status.InstanceProfiles.Previous, nodeClass.Status.InstanceProfiles.Current)
			}
			nodeClass.Status.InstanceProfiles.Current = profileName
			//nodeClass.Status.InstanceProfiles.Version++
		}
		// Run garbage collection on every reconciliation
		if err := ip.garbageCollectProfiles(ctx, nodeClass); err != nil {
			return reconcile.Result{}, fmt.Errorf("garbage collecting instance profiles: %w", err)
		}

	} else {
		nodeClass.Status.InstanceProfile = lo.FromPtr(nodeClass.Spec.InstanceProfile)
	}
	nodeClass.StatusConditions().SetTrue(v1.ConditionTypeInstanceProfileReady)
	return reconcile.Result{}, nil
}

func (ip *InstanceProfile) garbageCollectProfiles(ctx context.Context, nodeClass *v1.EC2NodeClass) error {
	if nodeClass.Status.InstanceProfiles == nil || len(nodeClass.Status.InstanceProfiles.Previous) == 0 {
		return nil
	}

	//ip.provisioner.cluster.Synced()??

	// Loops through previous profiles from newest to oldest
	for i := len(nodeClass.Status.InstanceProfiles.Previous) - 1; i >= 0; i-- {
		oldProfile := nodeClass.Status.InstanceProfiles.Previous[i]

		// Checks if any instances are using this profile
		instances, err := ip.ec2api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
			Filters: []ec2types.Filter{
				{
					Name:   aws.String("iam-instance-profile.arn"),
					Values: []string{fmt.Sprintf("*/%s", oldProfile)}, // matches any ARN that ends with profile name
				},
			},
		})
		if err != nil {
			return fmt.Errorf("checking instances using profile %s: %w", oldProfile, err)
		}

		// If no instances using this profile, delete profile
		if len(instances.Reservations) == 0 {
			if err := ip.instanceProfileProvider.Delete(ctx, oldProfile); err != nil {
				return fmt.Errorf("deleting instance profile %s: %w", oldProfile, err)
			}
			nodeClass.Status.InstanceProfiles.Previous = append(
				nodeClass.Status.InstanceProfiles.Previous[:i],
				nodeClass.Status.InstanceProfiles.Previous[i+1:]...,
			)
		}

	}
	return nil

}
