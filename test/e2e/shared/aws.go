//go:build e2e
// +build e2e

/*
Copyright 2020 The Kubernetes Authors.

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

package shared

import (
	"bytes"
	"context"
	b64 "encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	cfn "github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudtrail"
	"github.com/aws/aws-sdk-go/service/configservice"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecrpublic"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/servicequotas"
	cfn_iam "github.com/awslabs/goformation/v4/cloudformation/iam"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/yaml"

	cfn_bootstrap "sigs.k8s.io/cluster-api-provider-aws/cmd/clusterawsadm/cloudformation/bootstrap"
	cloudformation "sigs.k8s.io/cluster-api-provider-aws/cmd/clusterawsadm/cloudformation/service"
	"sigs.k8s.io/cluster-api-provider-aws/cmd/clusterawsadm/credentials"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/filter"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/services/wait"
)

type AWSInfrastructureSpec struct {
	ClusterName, VpcCidr, PublicSubnetCidr, PrivateSubnetCidr, AvailabilityZone string
	ExternalSecurityGroups                                                      bool
}

type AWSInfrastructureState struct {
	PrivateSubnetID     *string
	PrivateSubnetState  *string
	PublicSubnetID      *string
	PublicSubnetState   *string
	VpcState            *string
	NatGatewayState     *string
	PublicRouteTableID  *string
	PrivateRouteTableID *string
}

type AWSInfrastructure struct {
	Spec            AWSInfrastructureSpec
	Context         *E2EContext
	VPC             *ec2.Vpc
	Subnets         []*ec2.Subnet
	RouteTables     []*ec2.RouteTable
	InternetGateway *ec2.InternetGateway
	ElasticIP       *ec2.Address
	NatGateway      *ec2.NatGateway
	State           AWSInfrastructureState
}

func (i *AWSInfrastructure) New(ais AWSInfrastructureSpec, e2eCtx *E2EContext) AWSInfrastructure {
	i.Spec = ais
	i.Context = e2eCtx
	return *i
}

func (i *AWSInfrastructure) CreateVPC() AWSInfrastructure {
	cv, err := CreateVPC(i.Context, i.Spec.ClusterName+"-vpc", i.Spec.VpcCidr)
	if err != nil {
		return *i
	}

	i.VPC = cv
	i.State.VpcState = cv.State
	return *i
}

func (i *AWSInfrastructure) RefreshVPCState() AWSInfrastructure {
	vpc, err := GetVPC(i.Context, *i.VPC.VpcId)
	if err != nil {
		return *i
	}
	if vpc != nil {
		i.VPC = vpc
		i.State.VpcState = vpc.State
	}
	return *i
}

func (i *AWSInfrastructure) CreatePublicSubnet() AWSInfrastructure {
	subnet, err := CreateSubnet(i.Context, i.Spec.ClusterName, i.Spec.PublicSubnetCidr, i.Spec.AvailabilityZone, *i.VPC.VpcId, "public")
	if err != nil {
		i.State.PublicSubnetState = pointer.String("failed")
		return *i
	}
	i.State.PublicSubnetID = subnet.SubnetId
	i.State.PublicSubnetState = subnet.State
	i.Subnets = append(i.Subnets, subnet)
	return *i
}

func (i *AWSInfrastructure) CreatePrivateSubnet() AWSInfrastructure {
	subnet, err := CreateSubnet(i.Context, i.Spec.ClusterName, i.Spec.PrivateSubnetCidr, i.Spec.AvailabilityZone, *i.VPC.VpcId, "private")
	if err != nil {
		i.State.PrivateSubnetState = pointer.String("failed")
		return *i
	}
	i.State.PrivateSubnetID = subnet.SubnetId
	i.State.PrivateSubnetState = subnet.State
	i.Subnets = append(i.Subnets, subnet)
	return *i
}

func (i *AWSInfrastructure) CreateInternetGateway() AWSInfrastructure {
	igwC, err := CreateInternetGateway(i.Context, i.Spec.ClusterName+"-igw")
	if err != nil {
		return *i
	}
	_, aerr := AttachInternetGateway(i.Context, *igwC.InternetGatewayId, *i.VPC.VpcId)
	if aerr != nil {
		i.InternetGateway = igwC
		return *i
	}
	i.InternetGateway = igwC
	return *i
}

func (i *AWSInfrastructure) AllocateAddress() AWSInfrastructure {
	aa, err := AllocateAddress(i.Context, i.Spec.ClusterName+"-eip")
	if err != nil {
		return *i
	}

	if addr, _ := GetAddress(i.Context, *aa.AllocationId); addr != nil {
		i.ElasticIP = addr
	}
	return *i
}

func (i *AWSInfrastructure) CreateNatGateway(ct string) AWSInfrastructure {
	s, serr := GetSubnetByName(i.Context, i.Spec.ClusterName+"-subnet-"+ct)
	if serr != nil {
		return *i
	}
	ngwC, ngwce := CreateNatGateway(i.Context, i.Spec.ClusterName+"-nat", ct, *i.ElasticIP.AllocationId, *s.SubnetId)
	if ngwce != nil {
		return *i
	}
	if WaitForNatGatewayState(i.Context, *ngwC.NatGatewayId, 180, "available") {
		ngw, _ := GetNatGateway(i.Context, *ngwC.NatGatewayId)
		i.NatGateway = ngw
		i.State.NatGatewayState = ngw.State
		return *i
	}
	i.NatGateway = ngwC
	return *i
}

func (i *AWSInfrastructure) CreateRouteTable(subnetType string) AWSInfrastructure {
	rt, err := CreateRouteTable(i.Context, i.Spec.ClusterName+"-rt-"+subnetType, *i.VPC.VpcId)
	if err != nil {
		return *i
	}
	switch subnetType {
	case "public":
		if a, _ := AssociateRouteTable(i.Context, *rt.RouteTableId, *i.State.PublicSubnetID); a != nil {
			i.State.PublicRouteTableID = rt.RouteTableId
		}
	case "private":
		if a, _ := AssociateRouteTable(i.Context, *rt.RouteTableId, *i.State.PrivateSubnetID); a != nil {
			i.State.PrivateRouteTableID = rt.RouteTableId
		}
	}
	return *i
}

func (i *AWSInfrastructure) GetRouteTable(rtID string) AWSInfrastructure {
	rt, err := GetRouteTable(i.Context, rtID)
	if err != nil {
		return *i
	}
	if rt != nil {
		i.RouteTables = append(i.RouteTables, rt)
	}
	return *i
}

func (i *AWSInfrastructure) CreateInfrastructure() AWSInfrastructure {
	i.CreateVPC()
	Expect(i.VPC).NotTo(BeNil())
	if i.VPC != nil {
		i.CreatePublicSubnet()
		i.CreatePrivateSubnet()
		for t := 0; t < 30; t++ {
			if *i.RefreshVPCState().State.VpcState == "available" {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}
	i.CreateInternetGateway()
	i.AllocateAddress()
	i.CreateNatGateway("public")
	WaitForNatGatewayState(i.Context, *i.NatGateway.NatGatewayId, 180, "available")
	i.CreateRouteTable("public")
	i.CreateRouteTable("private")
	Expect(CreateRoute(i.Context, *i.State.PublicRouteTableID, "0.0.0.0/0", nil, i.InternetGateway.InternetGatewayId, nil)).To(BeTrue())
	Expect(CreateRoute(i.Context, *i.State.PrivateRouteTableID, "0.0.0.0/0", i.NatGateway.NatGatewayId, nil, nil)).To(BeTrue())
	i.GetRouteTable(*i.State.PublicRouteTableID)
	i.GetRouteTable(*i.State.PrivateRouteTableID)
	return *i
}

func (i *AWSInfrastructure) DeleteInfrastructure() AWSInfrastructure {
	for _, rt := range i.RouteTables {
		for _, a := range rt.Associations {
			DisassociateRouteTable(i.Context, *a.RouteTableAssociationId)
		}
		if !DeleteRouteTable(i.Context, *rt.RouteTableId) {
			fmt.Printf("%+v", rt)
		}
	}
	DeleteNatGateway(i.Context, *i.NatGateway.NatGatewayId)
	WaitForNatGatewayState(i.Context, *i.NatGateway.NatGatewayId, 180, "deleted")
	ReleaseAddress(i.Context, *i.ElasticIP.AllocationId)
	Eventually(DetachInternetGateway(i.Context, *i.InternetGateway.InternetGatewayId, *i.VPC.VpcId), 60*time.Second).Should(BeTrue())
	DeleteInternetGateway(i.Context, *i.InternetGateway.InternetGatewayId)
	DeleteSubnet(i.Context, *i.State.PrivateSubnetID)
	DeleteSubnet(i.Context, *i.State.PublicSubnetID)
	DeleteVPC(i.Context, *i.VPC.VpcId)
	return *i
}

func NewAWSSession() client.ConfigProvider {
	By("Getting an AWS IAM session - from environment")
	region, err := credentials.ResolveRegion("")
	Expect(err).NotTo(HaveOccurred())
	config := aws.NewConfig().WithCredentialsChainVerboseErrors(true).WithRegion(region)
	sess, err := session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
		Config:            *config,
	})
	Expect(err).NotTo(HaveOccurred())
	_, err = sess.Config.Credentials.Get()
	Expect(err).NotTo(HaveOccurred())
	return sess
}

func NewAWSSessionRepoWithKey(accessKey *iam.AccessKey) client.ConfigProvider {
	By("Getting an AWS IAM session - from access key")
	config := aws.NewConfig().WithCredentialsChainVerboseErrors(true).WithRegion("us-east-1")
	config.Credentials = awscreds.NewStaticCredentials(*accessKey.AccessKeyId, *accessKey.SecretAccessKey, "")

	sess, err := session.NewSessionWithOptions(session.Options{
		Config: *config,
	})
	Expect(err).NotTo(HaveOccurred())
	_, err = sess.Config.Credentials.Get()
	Expect(err).NotTo(HaveOccurred())
	return sess
}

func NewAWSSessionWithKey(accessKey *iam.AccessKey) client.ConfigProvider {
	By("Getting an AWS IAM session - from access key")
	region, err := credentials.ResolveRegion("")
	Expect(err).NotTo(HaveOccurred())
	config := aws.NewConfig().WithCredentialsChainVerboseErrors(true).WithRegion(region)
	config.Credentials = awscreds.NewStaticCredentials(*accessKey.AccessKeyId, *accessKey.SecretAccessKey, "")

	sess, err := session.NewSessionWithOptions(session.Options{
		Config: *config,
	})
	Expect(err).NotTo(HaveOccurred())
	_, err = sess.Config.Credentials.Get()
	Expect(err).NotTo(HaveOccurred())
	return sess
}

// createCloudFormationStack ensures the cloudformation stack is up to date.
func createCloudFormationStack(prov client.ConfigProvider, t *cfn_bootstrap.Template, tags map[string]string) error {
	Byf("Creating AWS CloudFormation stack for AWS IAM resources: stack-name=%s", t.Spec.StackName)
	CFN := cfn.New(prov)
	cfnSvc := cloudformation.NewService(CFN)

	err := cfnSvc.ReconcileBootstrapStack(t.Spec.StackName, *renderCustomCloudFormation(t), tags)
	if err != nil {
		stack, err := CFN.DescribeStacks(&cfn.DescribeStacksInput{StackName: aws.String(t.Spec.StackName)})
		if err == nil && len(stack.Stacks) > 0 {
			deleteMultitenancyRoles(prov)
			if aws.StringValue(stack.Stacks[0].StackStatus) == cfn.StackStatusRollbackFailed ||
				aws.StringValue(stack.Stacks[0].StackStatus) == cfn.StackStatusRollbackComplete ||
				aws.StringValue(stack.Stacks[0].StackStatus) == cfn.StackStatusRollbackInProgress {
				// If cloudformation stack creation fails due to resources that already exist, stack stays in rollback status and must be manually deleted.
				// Delete resources that failed because they already exists.
				deleteResourcesInCloudFormation(prov, t)
			}
		}
	}
	return err
}

func SetMultitenancyEnvVars(prov client.ConfigProvider) error {
	for _, roles := range MultiTenancyRoles {
		if err := roles.SetEnvVars(prov); err != nil {
			return err
		}
	}
	return nil
}

// Delete resources that already exists.
func deleteResourcesInCloudFormation(prov client.ConfigProvider, t *cfn_bootstrap.Template) {
	iamSvc := iam.New(prov)
	temp := *renderCustomCloudFormation(t)
	for _, val := range temp.Resources {
		tayp := val.AWSCloudFormationType()
		if tayp == configservice.ResourceTypeAwsIamRole {
			role := val.(*cfn_iam.Role)
			_, err := iamSvc.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(role.RoleName)})
			Expect(err).NotTo(HaveOccurred())
		}
		if val.AWSCloudFormationType() == "AWS::IAM::InstanceProfile" {
			profile := val.(*cfn_iam.InstanceProfile)
			_, _ = iamSvc.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(profile.InstanceProfileName)})
		}
		if val.AWSCloudFormationType() == "AWS::IAM::ManagedPolicy" {
			policy := val.(*cfn_iam.ManagedPolicy)
			policies, err := iamSvc.ListPolicies(&iam.ListPoliciesInput{})
			Expect(err).NotTo(HaveOccurred())
			if len(policies.Policies) > 0 {
				for _, p := range policies.Policies {
					if aws.StringValue(p.PolicyName) == policy.ManagedPolicyName {
						_, _ = iamSvc.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: p.Arn})
						break
					}
				}
			}
		}
		if val.AWSCloudFormationType() == configservice.ResourceTypeAwsIamGroup {
			group := val.(*cfn_iam.Group)
			_, _ = iamSvc.DeleteGroup(&iam.DeleteGroupInput{GroupName: aws.String(group.GroupName)})
		}
	}
}

// TODO: remove once test infra accounts are fixed.
func deleteMultitenancyRoles(prov client.ConfigProvider) {
	DeleteRole(prov, "multi-tenancy-role")
	DeleteRole(prov, "multi-tenancy-nested-role")
}

// detachAllPoliciesForRole detaches all policies for role.
func detachAllPoliciesForRole(prov client.ConfigProvider, name string) error {
	iamSvc := iam.New(prov)

	input := &iam.ListAttachedRolePoliciesInput{
		RoleName: &name,
	}
	policies, err := iamSvc.ListAttachedRolePolicies(input)
	if err != nil {
		return errors.New("error fetching policies for role")
	}
	for _, p := range policies.AttachedPolicies {
		input := &iam.DetachRolePolicyInput{
			RoleName:  aws.String(name),
			PolicyArn: p.PolicyArn,
		}

		_, err := iamSvc.DetachRolePolicy(input)
		if err != nil {
			return errors.New("failed detaching policy from a role")
		}
	}
	return nil
}

// DeleteRole deletes roles in a best effort manner.
func DeleteRole(prov client.ConfigProvider, name string) {
	iamSvc := iam.New(prov)

	// if role does not exist, return.
	_, err := iamSvc.GetRole(&iam.GetRoleInput{RoleName: aws.String(name)})
	if err != nil {
		return
	}

	if err := detachAllPoliciesForRole(prov, name); err != nil {
		return
	}

	_, err = iamSvc.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(name)})
	if err != nil {
		return
	}
}

func GetPolicyArn(prov client.ConfigProvider, name string) string {
	iamSvc := iam.New(prov)
	policyList, err := iamSvc.ListPolicies(&iam.ListPoliciesInput{
		Scope: aws.String(iam.PolicyScopeTypeLocal),
	})
	Expect(err).NotTo(HaveOccurred())

	for _, policy := range policyList.Policies {
		if aws.StringValue(policy.PolicyName) == name {
			return aws.StringValue(policy.Arn)
		}
	}
	return ""
}

// deleteCloudFormationStack removes the provisioned clusterawsadm stack.
func deleteCloudFormationStack(prov client.ConfigProvider, t *cfn_bootstrap.Template) {
	Byf("Deleting %s CloudFormation stack", t.Spec.StackName)
	CFN := cfn.New(prov)
	cfnSvc := cloudformation.NewService(CFN)
	err := cfnSvc.DeleteStack(t.Spec.StackName, nil)
	if err != nil {
		var retainResources []*string
		out, err := CFN.DescribeStackResources(&cfn.DescribeStackResourcesInput{StackName: aws.String(t.Spec.StackName)})
		Expect(err).NotTo(HaveOccurred())
		for _, v := range out.StackResources {
			if aws.StringValue(v.ResourceStatus) == cfn.ResourceStatusDeleteFailed {
				retainResources = append(retainResources, v.LogicalResourceId)
			}
		}
		err = cfnSvc.DeleteStack(t.Spec.StackName, retainResources)
		Expect(err).NotTo(HaveOccurred())
	}
	err = CFN.WaitUntilStackDeleteComplete(&cfn.DescribeStacksInput{
		StackName: aws.String(t.Spec.StackName),
	})
	Expect(err).NotTo(HaveOccurred())
}

func ensureTestImageUploaded(e2eCtx *E2EContext) error {
	sessionForRepo := NewAWSSessionRepoWithKey(e2eCtx.Environment.BootstrapAccessKey)

	ecrSvc := ecrpublic.New(sessionForRepo)
	repoName := ""
	if err := wait.WaitForWithRetryable(wait.NewBackoff(), func() (bool, error) {
		output, err := ecrSvc.CreateRepository(&ecrpublic.CreateRepositoryInput{
			RepositoryName: aws.String("capa/update"),
		})

		if err != nil {
			if !awserrors.IsRepositoryExists(err) {
				return false, err
			}
			out, err := ecrSvc.DescribeRepositories(&ecrpublic.DescribeRepositoriesInput{RepositoryNames: []*string{aws.String("capa/update")}})
			if err != nil || len(out.Repositories) == 0 {
				return false, err
			}
			repoName = aws.StringValue(out.Repositories[0].RepositoryUri)
		} else {
			repoName = aws.StringValue(output.Repository.RepositoryUri)
		}

		return true, nil
	}, awserrors.UnrecognizedClientException); err != nil {
		return err
	}

	cmd := exec.Command("docker", "inspect", "--format='{{index .Id}}'", "gcr.io/k8s-staging-cluster-api/capa-manager:e2e")
	var stdOut bytes.Buffer
	cmd.Stdout = &stdOut
	err := cmd.Run()
	if err != nil {
		return err
	}

	imageSha := strings.ReplaceAll(strings.TrimSuffix(stdOut.String(), "\n"), "'", "")

	ecrImageName := repoName + ":e2e"
	cmd = exec.Command("docker", "tag", imageSha, ecrImageName) //nolint:gosec
	err = cmd.Run()
	if err != nil {
		return err
	}

	outToken, err := ecrSvc.GetAuthorizationToken(&ecrpublic.GetAuthorizationTokenInput{})
	if err != nil {
		return err
	}

	// Auth token is in username:password format. To login using it, we need to decode first and separate password and username
	decodedUsernamePassword, _ := b64.StdEncoding.DecodeString(aws.StringValue(outToken.AuthorizationData.AuthorizationToken))

	strList := strings.Split(string(decodedUsernamePassword), ":")
	if len(strList) != 2 {
		return errors.New("failed to decode ECR authentication token")
	}

	cmd = exec.Command("docker", "login", "--username", strList[0], "--password", strList[1], "public.ecr.aws") //nolint:gosec
	err = cmd.Run()
	if err != nil {
		return err
	}

	cmd = exec.Command("docker", "push", ecrImageName)
	err = cmd.Run()
	if err != nil {
		return err
	}
	e2eCtx.E2EConfig.Variables["CAPI_IMAGES_REGISTRY"] = repoName
	e2eCtx.E2EConfig.Variables["E2E_IMAGE_TAG"] = "e2e"
	return nil
}

// ensureNoServiceLinkedRoles removes an auto-created IAM role, and tests
// the controller's IAM permissions to use ELB and Spot instances successfully.
func ensureNoServiceLinkedRoles(prov client.ConfigProvider) {
	Byf("Deleting AWS IAM Service Linked Role: role-name=AWSServiceRoleForElasticLoadBalancing")
	iamSvc := iam.New(prov)
	_, err := iamSvc.DeleteServiceLinkedRole(&iam.DeleteServiceLinkedRoleInput{
		RoleName: aws.String("AWSServiceRoleForElasticLoadBalancing"),
	})
	if code, _ := awserrors.Code(err); code != iam.ErrCodeNoSuchEntityException {
		Expect(err).NotTo(HaveOccurred())
	}

	Byf("Deleting AWS IAM Service Linked Role: role-name=AWSServiceRoleForEC2Spot")
	_, err = iamSvc.DeleteServiceLinkedRole(&iam.DeleteServiceLinkedRoleInput{
		RoleName: aws.String("AWSServiceRoleForEC2Spot"),
	})
	if code, _ := awserrors.Code(err); code != iam.ErrCodeNoSuchEntityException {
		Expect(err).NotTo(HaveOccurred())
	}
}

// ensureSSHKeyPair ensures A SSH key is present under the name.
func ensureSSHKeyPair(prov client.ConfigProvider, keyPairName string) {
	Byf("Ensuring presence of SSH key in EC2: key-name=%s", keyPairName)
	ec2c := ec2.New(prov)
	_, err := ec2c.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(keyPairName)})
	if code, _ := awserrors.Code(err); code != "InvalidKeyPair.Duplicate" {
		Expect(err).NotTo(HaveOccurred())
	}
}

func ensureStackTags(prov client.ConfigProvider, stackName string, expectedTags map[string]string) {
	Byf("Ensuring AWS CloudFormation stack is created or updated with the specified tags: stack-name=%s", stackName)
	CFN := cfn.New(prov)
	r, err := CFN.DescribeStacks(&cfn.DescribeStacksInput{StackName: &stackName})
	Expect(err).NotTo(HaveOccurred())
	stacks := r.Stacks
	Expect(len(stacks)).To(BeNumerically("==", 1))
	stackTags := stacks[0].Tags
	Expect(len(stackTags)).To(BeNumerically("==", len(expectedTags)))
	for _, tag := range stackTags {
		Expect(*tag.Value).To(BeIdenticalTo(expectedTags[*tag.Key]))
	}
}

// encodeCredentials leverages clusterawsadm to encode AWS credentials.
func encodeCredentials(accessKey *iam.AccessKey, region string) string {
	creds := credentials.AWSCredentials{
		Region:          region,
		AccessKeyID:     *accessKey.AccessKeyId,
		SecretAccessKey: *accessKey.SecretAccessKey,
	}
	encCreds, err := creds.RenderBase64EncodedAWSDefaultProfile()
	Expect(err).NotTo(HaveOccurred())
	return encCreds
}

// newUserAccessKey generates a new AWS Access Key pair based off of the
// bootstrap user. This tests that the CloudFormation policy is correct.
func newUserAccessKey(prov client.ConfigProvider, userName string) *iam.AccessKey {
	iamSvc := iam.New(prov)
	keyOuts, _ := iamSvc.ListAccessKeys(&iam.ListAccessKeysInput{
		UserName: aws.String(userName),
	})
	for i := range keyOuts.AccessKeyMetadata {
		Byf("Deleting an existing access key: user-name=%s", userName)
		_, err := iamSvc.DeleteAccessKey(&iam.DeleteAccessKeyInput{
			UserName:    aws.String(userName),
			AccessKeyId: keyOuts.AccessKeyMetadata[i].AccessKeyId,
		})
		Expect(err).NotTo(HaveOccurred())
	}
	Byf("Creating an access key: user-name=%s", userName)
	out, err := iamSvc.CreateAccessKey(&iam.CreateAccessKeyInput{UserName: aws.String(userName)})
	Expect(err).NotTo(HaveOccurred())
	Expect(out.AccessKey).ToNot(BeNil())

	return &iam.AccessKey{
		AccessKeyId:     out.AccessKey.AccessKeyId,
		SecretAccessKey: out.AccessKey.SecretAccessKey,
	}
}

func DumpCloudTrailEvents(e2eCtx *E2EContext) {
	client := cloudtrail.New(e2eCtx.BootstrapUserAWSSession)
	events := []*cloudtrail.Event{}
	err := client.LookupEventsPages(
		&cloudtrail.LookupEventsInput{
			StartTime: aws.Time(e2eCtx.StartOfSuite),
			EndTime:   aws.Time(time.Now()),
		},
		func(page *cloudtrail.LookupEventsOutput, lastPage bool) bool {
			events = append(events, page.Events...)
			return !lastPage
		},
	)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "couldn't get AWS CloudTrail events: err=%v", err)
	}
	logPath := filepath.Join(e2eCtx.Settings.ArtifactFolder, "cloudtrail-events.yaml")
	dat, err := yaml.Marshal(events)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "Failed to marshal AWS CloudTrail events: err=%v", err)
	}
	if err := os.WriteFile(logPath, dat, 0600); err != nil {
		fmt.Fprintf(GinkgoWriter, "couldn't write cloudtrail events to file: file=%s err=%s", logPath, err)
		return
	}
}

// conformanceImageID looks up a specific image for a given
// Kubernetes version in the e2econfig.
func conformanceImageID(e2eCtx *E2EContext) string {
	ver := e2eCtx.E2EConfig.GetVariable("CONFORMANCE_CI_ARTIFACTS_KUBERNETES_VERSION")
	strippedVer := strings.Replace(ver, "v", "", 1)
	amiName := AMIPrefix + strippedVer + "*"

	Byf("Searching for AMI: name=%s", amiName)
	ec2Svc := ec2.New(e2eCtx.AWSSession)
	filters := []*ec2.Filter{
		{
			Name:   aws.String("name"),
			Values: []*string{aws.String(amiName)},
		},
	}
	filters = append(filters, &ec2.Filter{
		Name:   aws.String("owner-id"),
		Values: []*string{aws.String(DefaultImageLookupOrg)},
	})
	resp, err := ec2Svc.DescribeImages(&ec2.DescribeImagesInput{
		Filters: filters,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(len(resp.Images)).To(Not(BeZero()))
	imageID := aws.StringValue(resp.Images[0].ImageId)
	Byf("Using AMI: image-id=%s", imageID)
	return imageID
}

func GetAvailabilityZones(sess client.ConfigProvider) []*ec2.AvailabilityZone {
	ec2Client := ec2.New(sess)
	azs, err := ec2Client.DescribeAvailabilityZones(nil)
	Expect(err).NotTo(HaveOccurred())
	return azs.AvailabilityZones
}

type ServiceQuota struct {
	ServiceCode         string
	QuotaName           string
	QuotaCode           string
	Value               int
	DesiredMinimumValue int
	RequestStatus       string
}

func EnsureServiceQuotas(sess client.ConfigProvider) map[string]*ServiceQuota {
	limitedResources := getLimitedResources()
	serviceQuotasClient := servicequotas.New(sess)

	for k, v := range limitedResources {
		out, err := serviceQuotasClient.GetServiceQuota(&servicequotas.GetServiceQuotaInput{
			QuotaCode:   aws.String(v.QuotaCode),
			ServiceCode: aws.String(v.ServiceCode),
		})
		Expect(err).NotTo(HaveOccurred())
		v.Value = int(aws.Float64Value(out.Quota.Value))
		limitedResources[k] = v
		if v.Value < v.DesiredMinimumValue {
			v.attemptRaiseServiceQuotaRequest(serviceQuotasClient)
		}
	}

	return limitedResources
}

func (s *ServiceQuota) attemptRaiseServiceQuotaRequest(serviceQuotasClient *servicequotas.ServiceQuotas) {
	s.updateServiceQuotaRequestStatus(serviceQuotasClient)
	if s.RequestStatus == "" {
		s.raiseServiceRequest(serviceQuotasClient)
	}
}

func (s *ServiceQuota) raiseServiceRequest(serviceQuotasClient *servicequotas.ServiceQuotas) {
	fmt.Printf("Requesting service quota increase for %s/%s to %d\n", s.ServiceCode, s.QuotaName, s.DesiredMinimumValue)
	out, err := serviceQuotasClient.RequestServiceQuotaIncrease(
		&servicequotas.RequestServiceQuotaIncreaseInput{
			DesiredValue: aws.Float64(float64(s.DesiredMinimumValue)),
			ServiceCode:  aws.String(s.ServiceCode),
			QuotaCode:    aws.String(s.QuotaCode),
		},
	)
	if err != nil {
		fmt.Printf("Unable to raise quota for %s/%s: %s\n", s.ServiceCode, s.QuotaName, err)
	} else {
		s.RequestStatus = aws.StringValue(out.RequestedQuota.Status)
	}
}

func (s *ServiceQuota) updateServiceQuotaRequestStatus(serviceQuotasClient *servicequotas.ServiceQuotas) {
	params := &servicequotas.ListRequestedServiceQuotaChangeHistoryInput{
		ServiceCode: aws.String(s.ServiceCode),
	}
	latestRequest := &servicequotas.RequestedServiceQuotaChange{}
	_ = serviceQuotasClient.ListRequestedServiceQuotaChangeHistoryPages(params,
		func(page *servicequotas.ListRequestedServiceQuotaChangeHistoryOutput, lastPage bool) bool {
			for _, v := range page.RequestedQuotas {
				if int(aws.Float64Value(v.DesiredValue)) >= s.DesiredMinimumValue && aws.StringValue(v.QuotaCode) == s.QuotaCode && aws.TimeValue(v.Created).After(aws.TimeValue(latestRequest.Created)) {
					latestRequest = v
				}
			}
			return !lastPage
		},
	)
	if latestRequest.Status != nil {
		s.RequestStatus = aws.StringValue(latestRequest.Status)
	}
}

func DumpEKSClusters(ctx context.Context, e2eCtx *E2EContext) {
	logPath := filepath.Join(e2eCtx.Settings.ArtifactFolder, "clusters", e2eCtx.Environment.BootstrapClusterProxy.GetName(), "aws-resources")
	if err := os.MkdirAll(logPath, os.ModePerm); err != nil {
		fmt.Fprintf(GinkgoWriter, "couldn't create directory: path=%s, err=%s", logPath, err)
	}
	fmt.Fprintf(GinkgoWriter, "folder created for eks clusters: %s\n", logPath)

	input := &eks.ListClustersInput{}
	eksClient := eks.New(e2eCtx.BootstrapUserAWSSession)
	output, err := eksClient.ListClusters(input)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "couldn't list EKS clusters: err=%s", err)
		return
	}

	for _, clusterName := range output.Clusters {
		describeInput := &eks.DescribeClusterInput{
			Name: clusterName,
		}
		describeOutput, err := eksClient.DescribeCluster(describeInput)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "couldn't describe EKS clusters: name=%s err=%s", *clusterName, err)
			continue
		}
		dumpEKSCluster(describeOutput.Cluster, logPath)
	}
}

func dumpEKSCluster(cluster *eks.Cluster, logPath string) {
	clusterYAML, err := yaml.Marshal(cluster)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "couldn't marshal cluster to yaml: name=%s err=%s", *cluster.Name, err)
		return
	}

	fileName := fmt.Sprintf("%s.yaml", *cluster.Name)
	clusterLog := path.Join(logPath, fileName)
	f, err := os.OpenFile(clusterLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, os.ModePerm) //nolint:gosec
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "couldn't open log file: name=%s err=%s", clusterLog, err)
		return
	}
	defer f.Close() //nolint:gosec

	if err := os.WriteFile(f.Name(), clusterYAML, 0600); err != nil {
		fmt.Fprintf(GinkgoWriter, "couldn't write cluster yaml to file: name=%s file=%s err=%s", *cluster.Name, f.Name(), err)
		return
	}
}

// To calculate how much resources a test consumes, these helper functions below can be used.
// ListVpcInternetGateways, ListNATGateways, ListRunningEC2, ListVPC

func ListVpcInternetGateways(e2eCtx *E2EContext) ([]*ec2.InternetGateway, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	out, err := ec2Svc.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{})
	if err != nil {
		return nil, err
	}

	return out.InternetGateways, nil
}

func ListNATGateways(e2eCtx *E2EContext) (map[string]*ec2.NatGateway, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	describeNatGatewayInput := &ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{
			filter.EC2.NATGatewayStates(ec2.NatGatewayStateAvailable),
		},
	}

	gateways := make(map[string]*ec2.NatGateway)

	err := ec2Svc.DescribeNatGatewaysPages(describeNatGatewayInput,
		func(page *ec2.DescribeNatGatewaysOutput, lastPage bool) bool {
			for _, r := range page.NatGateways {
				gateways[*r.SubnetId] = r
			}
			return !lastPage
		})
	if err != nil {
		return nil, err
	}

	return gateways, nil
}

func ListRunningEC2(e2eCtx *E2EContext) ([]instance, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	resp, err := ec2Svc.DescribeInstancesWithContext(context.TODO(), &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{filter.EC2.InstanceStates(ec2.InstanceStateNameRunning)},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
		return nil, fmt.Errorf("no machines found")
	}
	instances := []instance{}
	for _, r := range resp.Reservations {
		for _, i := range r.Instances {
			tags := i.Tags
			name := ""
			for _, t := range tags {
				if aws.StringValue(t.Key) == "Name" {
					name = aws.StringValue(t.Value)
				}
			}
			if name == "" {
				continue
			}
			instances = append(instances,
				instance{
					name:       name,
					instanceID: aws.StringValue(i.InstanceId),
				},
			)
		}
	}
	return instances, nil
}

func ListClusterEC2Instances(e2eCtx *E2EContext, clusterName string) ([]*ec2.Instance, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)
	filter := &ec2.Filter{
		Name:   aws.String("tag-key"),
		Values: aws.StringSlice([]string{"sigs.k8s.io/cluster-api-provider-aws/cluster/" + clusterName}),
	}
	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeInstances(input)
	if err != nil {
		return nil, err
	}
	instances := []*ec2.Instance{}
	for _, r := range result.Reservations {
		instances = append(instances, r.Instances...)
	}
	return instances, nil
}

func WaitForInstanceState(e2eCtx *E2EContext, clusterName string, timeout int, state string) bool {
	t := 0
	for t < timeout {
		st := map[string]int{
			"pending":       0,
			"running":       0,
			"shutting-down": 0,
			"terminated":    0,
		}
		instances, _ := ListClusterEC2Instances(e2eCtx, clusterName)
		for _, i := range instances {
			iState := *i.State.Name
			st[iState]++
		}
		if st[state] == len(instances) || len(instances) == 0 {
			return true
		}
		time.Sleep(1 * time.Second)
		t++
	}
	return false
}

func ListVPC(e2eCtx *E2EContext) int {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			filter.EC2.VPCStates(ec2.VpcStateAvailable),
		},
	}

	out, err := ec2Svc.DescribeVpcs(input)
	if err != nil {
		return 0
	}

	return len(out.Vpcs)
}

func GetVPC(e2eCtx *E2EContext, vpcID string) (*ec2.Vpc, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("vpc-id"),
		Values: aws.StringSlice([]string{vpcID}),
	}

	input := &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeVpcs(input)
	if err != nil {
		return nil, err
	}
	if result.Vpcs == nil {
		return nil, nil
	}
	return result.Vpcs[0], nil
}

func CreateVPC(e2eCtx *E2EContext, vpcName string, cidrBlock string) (*ec2.Vpc, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.CreateVpcInput{
		CidrBlock: aws.String(cidrBlock),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(vpcName),
					},
				},
			},
		},
	}
	result, err := ec2Svc.CreateVpc(input)
	if err != nil {
		return nil, err
	}
	return result.Vpc, nil
}

func DisassociateVpcCidrBlock(e2eCtx *E2EContext, assocID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DisassociateVpcCidrBlockInput{
		AssociationId: aws.String(assocID),
	}

	if _, err := ec2Svc.DisassociateVpcCidrBlock(input); err != nil {
		return false
	}
	return true
}

func DeleteVPC(e2eCtx *E2EContext, vpcID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DeleteVpcInput{
		VpcId: aws.String(vpcID),
	}
	if _, err := ec2Svc.DeleteVpc(input); err != nil {
		return false
	}
	return true
}

func ListVpcSubnets(e2eCtx *E2EContext, vpcID string) ([]*ec2.Subnet, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("vpc-id"),
		Values: aws.StringSlice([]string{vpcID}),
	}

	input := &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeSubnets(input)
	if err != nil {
		return nil, err
	}
	return result.Subnets, nil
}

func GetSubnet(e2eCtx *E2EContext, subnetID string) (*ec2.Subnet, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("subnet-id"),
		Values: aws.StringSlice([]string{subnetID}),
	}

	input := &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeSubnets(input)
	if err != nil {
		return nil, err
	}
	if result.Subnets == nil {
		return nil, nil
	}
	return result.Subnets[0], nil
}

func GetSubnetByName(e2eCtx *E2EContext, name string) (*ec2.Subnet, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("tag:Name"),
		Values: aws.StringSlice([]string{name}),
	}

	input := &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeSubnets(input)
	if err != nil {
		return nil, err
	}
	if result.Subnets == nil {
		return nil, nil
	}
	return result.Subnets[0], nil
}

func CreateSubnet(e2eCtx *E2EContext, clusterName string, cidrBlock string, az string, vpcID string, st string) (*ec2.Subnet, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.CreateSubnetInput{
		CidrBlock: aws.String(cidrBlock),
		VpcId:     aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("subnet"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(clusterName + "-subnet-" + st),
					},
					{
						Key:   aws.String("kubernetes.io/cluster/" + clusterName),
						Value: aws.String("shared"),
					},
				},
			},
		},
	}

	// Tag subnet based on type(st)
	switch st {
	case "private":
		input.TagSpecifications[0].Tags = append(input.TagSpecifications[0].Tags, &ec2.Tag{
			Key:   aws.String("kubernetes.io/role/internal-elb"),
			Value: aws.String("1"),
		})
	case "public":
		input.TagSpecifications[0].Tags = append(input.TagSpecifications[0].Tags, &ec2.Tag{
			Key:   aws.String("kubernetes.io/role/elb"),
			Value: aws.String("1"),
		})
	}

	if az != "" {
		input.AvailabilityZone = aws.String(az)
	}

	result, err := ec2Svc.CreateSubnet(input)
	if err != nil {
		return nil, err
	}
	return result.Subnet, nil
}

func DeleteSubnet(e2eCtx *E2EContext, subnetID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DeleteSubnetInput{
		SubnetId: aws.String(subnetID),
	}

	if _, err := ec2Svc.DeleteSubnet(input); err != nil {
		return false
	}
	return true
}

func GetAddress(e2eCtx *E2EContext, allocationID string) (*ec2.Address, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("allocation-id"),
		Values: aws.StringSlice([]string{allocationID}),
	}

	input := &ec2.DescribeAddressesInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeAddresses(input)
	if err != nil {
		return nil, err
	}
	if result.Addresses == nil {
		return nil, nil
	}
	return result.Addresses[0], nil
}

func AllocateAddress(e2eCtx *E2EContext, eipName string) (*ec2.AllocateAddressOutput, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.AllocateAddressInput{
		Domain: aws.String("vpc"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("elastic-ip"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(eipName),
					},
				},
			},
		},
	}

	result, err := ec2Svc.AllocateAddress(input)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func DisassociateAddress(e2eCtx *E2EContext, assocID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DisassociateAddressInput{
		AssociationId: aws.String(assocID),
	}

	if _, err := ec2Svc.DisassociateAddress(input); err != nil {
		return false
	}
	return true
}

func ReleaseAddress(e2eCtx *E2EContext, allocationID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.ReleaseAddressInput{
		AllocationId: aws.String(allocationID),
	}

	if _, err := ec2Svc.ReleaseAddress(input); err != nil {
		return false
	}
	return true
}

func CreateNatGateway(e2eCtx *E2EContext, gatewayName string, connectType string, allocationID string, subnetID string) (*ec2.NatGateway, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.CreateNatGatewayInput{
		SubnetId: aws.String(subnetID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("natgateway"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(gatewayName),
					},
				},
			},
		},
	}

	if connectType != "" {
		input.ConnectivityType = aws.String(connectType)
	}

	if allocationID != "" {
		input.AllocationId = aws.String(allocationID)
	}

	result, err := ec2Svc.CreateNatGateway(input)
	if err != nil {
		return nil, err
	}
	return result.NatGateway, nil
}

func GetNatGateway(e2eCtx *E2EContext, gatewayID string) (*ec2.NatGateway, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("nat-gateway-id"),
		Values: aws.StringSlice([]string{gatewayID}),
	}

	input := &ec2.DescribeNatGatewaysInput{
		Filter: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeNatGateways(input)
	if err != nil {
		return nil, err
	}
	if result.NatGateways == nil {
		return nil, nil
	}
	return result.NatGateways[0], nil
}

func DeleteNatGateway(e2eCtx *E2EContext, gatewayID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(gatewayID),
	}

	if _, err := ec2Svc.DeleteNatGateway(input); err != nil {
		return false
	}
	return true
}

func WaitForNatGatewayState(e2eCtx *E2EContext, gatewayID string, timeout int, state string) bool {
	t := 0
	for t < timeout {
		gw, _ := GetNatGateway(e2eCtx, gatewayID)
		gwState := *gw.State
		if gwState == state {
			return true
		}
		time.Sleep(1 * time.Second)
		t++
	}
	return false
}

func CreateInternetGateway(e2eCtx *E2EContext, gatewayName string) (*ec2.InternetGateway, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.CreateInternetGatewayInput{
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("internet-gateway"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(gatewayName),
					},
				},
			},
		},
	}

	result, err := ec2Svc.CreateInternetGateway(input)
	if err != nil {
		return nil, err
	}
	return result.InternetGateway, nil
}

func GetInternetGateway(e2eCtx *E2EContext, gatewayID string) (*ec2.InternetGateway, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("internet-gateway-id"),
		Values: aws.StringSlice([]string{gatewayID}),
	}

	input := &ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeInternetGateways(input)
	if err != nil {
		return nil, err
	}
	if result.InternetGateways == nil {
		return nil, nil
	}
	return result.InternetGateways[0], nil
}

func DeleteInternetGateway(e2eCtx *E2EContext, gatewayID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(gatewayID),
	}

	if _, err := ec2Svc.DeleteInternetGateway(input); err != nil {
		return false
	}
	return true
}

func AttachInternetGateway(e2eCtx *E2EContext, gatewayID string, vpcID string) (bool, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(gatewayID),
		VpcId:             aws.String(vpcID),
	}

	if _, err := ec2Svc.AttachInternetGateway(input); err != nil {
		return false, err
	}
	return true, nil
}

func DetachInternetGateway(e2eCtx *E2EContext, gatewayID string, vpcID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(gatewayID),
		VpcId:             aws.String(vpcID),
	}

	if _, err := ec2Svc.DetachInternetGateway(input); err != nil {
		return false
	}
	return true
}

func CreatePeering(e2eCtx *E2EContext, peerName string, vpcID string, peerVpcID string) (*ec2.VpcPeeringConnection, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.CreateVpcPeeringConnectionInput{
		VpcId:     aws.String(vpcID),
		PeerVpcId: aws.String(peerVpcID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc-peering-connection"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(peerName),
					},
				},
			},
		},
	}

	result, err := ec2Svc.CreateVpcPeeringConnection(input)
	if err != nil {
		return nil, err
	}
	return result.VpcPeeringConnection, nil
}

func GetPeering(e2eCtx *E2EContext, peeringID string) (*ec2.VpcPeeringConnection, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("vpc-peering-connection-id"),
		Values: aws.StringSlice([]string{peeringID}),
	}

	input := &ec2.DescribeVpcPeeringConnectionsInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeVpcPeeringConnections(input)
	if err != nil {
		return nil, err
	}
	if result.VpcPeeringConnections == nil {
		return nil, nil
	}
	return result.VpcPeeringConnections[0], nil
}

func DeletePeering(e2eCtx *E2EContext, peeringID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DeleteVpcPeeringConnectionInput{
		VpcPeeringConnectionId: aws.String(peeringID),
	}

	if _, err := ec2Svc.DeleteVpcPeeringConnection(input); err != nil {
		return false
	}
	return true
}

func AcceptPeering(e2eCtx *E2EContext, peeringID string) (*ec2.VpcPeeringConnection, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.AcceptVpcPeeringConnectionInput{
		VpcPeeringConnectionId: aws.String(peeringID),
	}

	result, err := ec2Svc.AcceptVpcPeeringConnection(input)
	if err != nil {
		return nil, err
	}
	return result.VpcPeeringConnection, nil
}

func CreateRouteTable(e2eCtx *E2EContext, rtName string, vpcID string) (*ec2.RouteTable, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.CreateRouteTableInput{
		VpcId: aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("route-table"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(rtName),
					},
				},
			},
		},
	}

	result, err := ec2Svc.CreateRouteTable(input)
	if err != nil {
		return nil, err
	}
	return result.RouteTable, nil
}

func ListVpcRouteTables(e2eCtx *E2EContext, vpcID string) ([]*ec2.RouteTable, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("vpc-id"),
		Values: aws.StringSlice([]string{vpcID}),
	}

	input := &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeRouteTables(input)
	if err != nil {
		return nil, err
	}
	return result.RouteTables, nil
}

func ListSubnetRouteTables(e2eCtx *E2EContext, subnetID string) ([]*ec2.RouteTable, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("association.subnet-id"),
		Values: aws.StringSlice([]string{subnetID}),
	}

	input := &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeRouteTables(input)
	if err != nil {
		return nil, err
	}
	return result.RouteTables, nil
}

func GetRouteTable(e2eCtx *E2EContext, rtID string) (*ec2.RouteTable, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("route-table-id"),
		Values: aws.StringSlice([]string{rtID}),
	}

	input := &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeRouteTables(input)
	if err != nil {
		return nil, err
	}
	if result.RouteTables == nil {
		return nil, nil
	}
	return result.RouteTables[0], nil
}

func DeleteRouteTable(e2eCtx *E2EContext, rtID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(rtID),
	}

	if _, err := ec2Svc.DeleteRouteTable(input); err != nil {
		return false
	}
	return true
}

func CreateRoute(e2eCtx *E2EContext, rtID string, destinationCidr string, natID *string, igwID *string, pcxID *string) (bool, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.CreateRouteInput{
		RouteTableId:         &rtID,
		DestinationCidrBlock: aws.String(destinationCidr),
	}

	if natID != nil {
		input.NatGatewayId = natID
	}

	if igwID != nil {
		input.GatewayId = igwID
	}

	if pcxID != nil {
		input.VpcPeeringConnectionId = pcxID
	}

	result, err := ec2Svc.CreateRoute(input)
	if err != nil {
		return false, err
	}
	return *result.Return, nil
}

func DeleteRoute(e2eCtx *E2EContext, rtID string, destinationCidr string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DeleteRouteInput{
		RouteTableId:         aws.String(rtID),
		DestinationCidrBlock: aws.String(destinationCidr),
	}

	if _, err := ec2Svc.DeleteRoute(input); err != nil {
		return false
	}
	return true
}

func AssociateRouteTable(e2eCtx *E2EContext, rtID string, subnetID string) (*ec2.AssociateRouteTableOutput, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.AssociateRouteTableInput{
		RouteTableId: aws.String(rtID),
		SubnetId:     aws.String(subnetID),
	}

	result, err := ec2Svc.AssociateRouteTable(input)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func DisassociateRouteTable(e2eCtx *E2EContext, assocID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DisassociateRouteTableInput{
		AssociationId: aws.String(assocID),
	}

	if _, err := ec2Svc.DisassociateRouteTable(input); err != nil {
		return false
	}
	return true
}

func CreateSecurityGroup(e2eCtx *E2EContext, sgName string, sgDescription string, vpcID string) (*ec2.CreateSecurityGroupOutput, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.CreateSecurityGroupInput{
		VpcId:       aws.String(vpcID),
		GroupName:   aws.String(sgName),
		Description: aws.String(sgDescription),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("security-group"),
				Tags: []*ec2.Tag{
					{
						Key:   aws.String("Name"),
						Value: aws.String(sgName),
					},
				},
			},
		},
	}

	result, err := ec2Svc.CreateSecurityGroup(input)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func GetSecurityGroup(e2eCtx *E2EContext, sgID string) (*ec2.SecurityGroup, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("group-id"),
		Values: aws.StringSlice([]string{sgID}),
	}

	input := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeSecurityGroups(input)
	if err != nil {
		return nil, err
	}
	if result.SecurityGroups == nil {
		return nil, nil
	}
	return result.SecurityGroups[0], nil
}

func DeleteSecurityGroup(e2eCtx *E2EContext, sgID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}

	if _, err := ec2Svc.DeleteSecurityGroup(input); err != nil {
		return false
	}
	return true
}

func ListSecurityGroupRules(e2eCtx *E2EContext, sgID string) ([]*ec2.SecurityGroupRule, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("group-id"),
		Values: aws.StringSlice([]string{sgID}),
	}

	input := &ec2.DescribeSecurityGroupRulesInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeSecurityGroupRules(input)
	if err != nil {
		return nil, err
	}
	return result.SecurityGroupRules, nil
}

func GetSecurityGroupRule(e2eCtx *E2EContext, sgrID string) (*ec2.SecurityGroupRule, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	filter := &ec2.Filter{
		Name:   aws.String("security-group-rule-id"),
		Values: aws.StringSlice([]string{sgrID}),
	}

	input := &ec2.DescribeSecurityGroupRulesInput{
		Filters: []*ec2.Filter{
			filter,
		},
	}

	result, err := ec2Svc.DescribeSecurityGroupRules(input)
	if err != nil {
		return nil, err
	}
	if result.SecurityGroupRules == nil {
		return nil, nil
	}
	return result.SecurityGroupRules[0], nil
}

func CreateSecurityGroupIngressRule(e2eCtx *E2EContext, sgID string, sgrDescription string, cidr string, protocol string, fromPort int64, toPort int64) (bool, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	ipPerm := &ec2.IpPermission{
		FromPort:   aws.Int64(fromPort),
		ToPort:     aws.Int64(toPort),
		IpProtocol: aws.String(protocol),
		IpRanges: []*ec2.IpRange{
			{
				CidrIp:      aws.String(cidr),
				Description: aws.String(sgrDescription),
			},
		},
	}

	input := &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{
			ipPerm,
		},
	}

	result, err := ec2Svc.AuthorizeSecurityGroupIngress(input)
	if err != nil {
		return false, err
	}
	return *result.Return, nil
}

func CreateSecurityGroupEgressRule(e2eCtx *E2EContext, sgID string, sgrDescription string, cidr string, protocol string, fromPort int64, toPort int64) (bool, error) {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	ipPerm := &ec2.IpPermission{
		FromPort:   aws.Int64(fromPort),
		ToPort:     aws.Int64(toPort),
		IpProtocol: aws.String(protocol),
		IpRanges: []*ec2.IpRange{
			{
				CidrIp:      aws.String(cidr),
				Description: aws.String(sgrDescription),
			},
		},
	}

	input := &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{
			ipPerm,
		},
	}
	result, err := ec2Svc.AuthorizeSecurityGroupEgress(input)
	if err != nil {
		return false, err
	}
	return *result.Return, nil
}

func CreateSecurityGroupRule(e2eCtx *E2EContext, sgID string, sgrDescription string, cidr string, protocol string, fromPort int64, toPort int64, rt string) (bool, error) {
	switch rt {
	case "ingress":
		return CreateSecurityGroupIngressRule(e2eCtx, sgID, sgrDescription, cidr, protocol, fromPort, toPort)
	case "egress":
		return CreateSecurityGroupEgressRule(e2eCtx, sgID, sgrDescription, cidr, protocol, fromPort, toPort)
	}
	return false, nil
}

func DeleteSecurityGroupIngressRule(e2eCtx *E2EContext, sgrID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.RevokeSecurityGroupIngressInput{
		SecurityGroupRuleIds: aws.StringSlice([]string{sgrID}),
	}

	if _, err := ec2Svc.RevokeSecurityGroupIngress(input); err != nil {
		return false
	}
	return true
}

func DeleteSecurityGroupEgressRule(e2eCtx *E2EContext, sgrID string) bool {
	ec2Svc := ec2.New(e2eCtx.AWSSession)

	input := &ec2.RevokeSecurityGroupEgressInput{
		SecurityGroupRuleIds: aws.StringSlice([]string{sgrID}),
	}

	if _, err := ec2Svc.RevokeSecurityGroupEgress(input); err != nil {
		return false
	}
	return true
}

func DeleteSecurityGroupRule(e2eCtx *E2EContext, sgrID string, rt string) bool {
	switch rt {
	case "ingress":
		return DeleteSecurityGroupIngressRule(e2eCtx, sgrID)
	case "egress":
		return DeleteSecurityGroupEgressRule(e2eCtx, sgrID)
	}
	return false
}
