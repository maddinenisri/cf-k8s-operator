/*
Copyright 2021.

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

package controllers

import (
	"context"
	coreerrors "errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudformation/cloudformationiface"

	cloudformationv1alpha1 "github.com/maddinenisri/cf-k8s-operator/api/v1alpha1"
)

const (
	controllerKey   = "kubernetes.io/controlled-by"
	controllerValue = "cloudformation.linki.space/operator"
	stacksFinalizer = "finalizer.cloudformation.mdstechinc.com"
	ownerKey        = "kubernetes.io/owned-by"
)

var (
	// log    *logrus.Logger
	// logger *logrus.Entry
	StackFlagSet     *pflag.FlagSet
	ErrStackNotFound = coreerrors.New("stack not found")
)

// StackReconciler reconciles a Stack object
type StackReconciler struct {
	client.Client
	Log                 logr.Logger
	Scheme              *runtime.Scheme
	cf                  cloudformationiface.CloudFormationAPI
	defaultTags         map[string]string
	defaultCapabilities []string
	dryRun              bool
}

func init() {
	StackFlagSet = pflag.NewFlagSet("stack", pflag.ExitOnError)

	StackFlagSet.String("region", "eu-central-1", "The AWS region to use")
	StackFlagSet.String("assume-role", "", "Assume AWS role when defined. Useful for stacks in another AWS account. Specify the full ARN, e.g. `arn:aws:iam::123456789:role/cloudformation-operator`")
	StackFlagSet.StringToString("tag", map[string]string{}, "Tags to apply to all Stacks by default. Specify multiple times for multiple tags.")
	StackFlagSet.StringSlice("capability", []string{}, "The AWS CloudFormation capability to enable")
	StackFlagSet.Bool("dry-run", false, "If true, don't actually do anything.")
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Stack object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile

// +kubebuilder:rbac:groups=cloudformation.mdstechinc.com,resources=stacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloudformation.mdstechinc.com,resources=stacks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloudformation.mdstechinc.com,resources=stacks/finalizers,verbs=update

func (r *StackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("stack", req.NamespacedName)
	log.Info("Reconciling Stack")

	// Fetch the Stack instance
	instance := &cloudformationv1alpha1.Stack{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("Stack resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get Stack")
		return ctrl.Result{}, err
	}

	// Check if the Stack instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isStackMarkedToBeDeleted := instance.GetDeletionTimestamp() != nil
	if isStackMarkedToBeDeleted {
		if contains(instance.GetFinalizers(), stacksFinalizer) {
			if err := r.finalizeStacks(log, instance); err != nil {
				return ctrl.Result{}, err
			}

			instance.SetFinalizers(remove(instance.GetFinalizers(), stacksFinalizer))
			err := r.Client.Update(ctx, instance)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer for this CR
	if !contains(instance.GetFinalizers(), stacksFinalizer) {
		if err := r.addFinalizer(log, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	exists, err := r.stackExists(instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	if exists {
		return ctrl.Result{}, r.updateStack(log, instance)
	}

	return ctrl.Result{}, r.createStack(log, instance)
}

func (r *StackReconciler) createStack(log logr.Logger, stack *cloudformationv1alpha1.Stack) error {
	logrus.WithField("stack", stack.Name).Info("creating stack")

	if r.dryRun {
		logrus.WithField("stack", stack.Name).Info("skipping stack creation")
		return nil
	}

	hasOwnership, err := r.hasOwnership(stack)
	if err != nil {
		return err
	}

	if !hasOwnership {
		logrus.WithField("stack", stack.Name).Info("no ownerhsip")
		return nil
	}

	stackTags, err := r.stackTags(stack)
	if err != nil {
		logrus.WithField("stack", stack.Name).Error("error compiling tags")
		return err
	}

	input := &cloudformation.CreateStackInput{
		Capabilities: aws.StringSlice(r.defaultCapabilities),
		StackName:    aws.String(stack.Name),
		TemplateBody: aws.String(stack.Spec.Template),
		Parameters:   r.stackParameters(stack),
		Tags:         stackTags,
	}

	if _, err := r.cf.CreateStack(input); err != nil {
		return err
	}

	if err := r.waitWhile(stack, cloudformation.StackStatusCreateInProgress); err != nil {
		return err
	}

	return r.updateStackStatus(stack)
}

func (r *StackReconciler) updateStack(log logr.Logger, stack *cloudformationv1alpha1.Stack) error {
	logrus.WithField("stack", stack.Name).Info("updating stack")

	if r.dryRun {
		logrus.WithField("stack", stack.Name).Info("skipping stack update")
		return nil
	}

	hasOwnership, err := r.hasOwnership(stack)
	if err != nil {
		return err
	}

	if !hasOwnership {
		logrus.WithField("stack", stack.Name).Info("no ownerhsip")
		return nil
	}

	stackTags, err := r.stackTags(stack)
	if err != nil {
		logrus.WithField("stack", stack.Name).Error("error compiling tags")
		return err
	}

	input := &cloudformation.UpdateStackInput{
		Capabilities: aws.StringSlice(r.defaultCapabilities),
		StackName:    aws.String(stack.Name),
		TemplateBody: aws.String(stack.Spec.Template),
		Parameters:   r.stackParameters(stack),
		Tags:         stackTags,
	}

	if _, err := r.cf.UpdateStack(input); err != nil {
		if strings.Contains(err.Error(), "No updates are to be performed.") {
			logrus.WithField("stack", stack.Name).Debug("stack already updated")
			return nil
		}
		return err
	}

	if err := r.waitWhile(stack, cloudformation.StackStatusUpdateInProgress); err != nil {
		return err
	}

	return r.updateStackStatus(stack)
}

func (r *StackReconciler) deleteStack(log logr.Logger, stack *cloudformationv1alpha1.Stack) error {
	logrus.WithField("stack", stack.Name).Info("deleting stack")

	if r.dryRun {
		logrus.WithField("stack", stack.Name).Info("skipping stack deletion")
		return nil
	}

	hasOwnership, err := r.hasOwnership(stack)
	if err != nil {
		return err
	}

	if !hasOwnership {
		logrus.WithField("stack", stack.Name).Info("no ownerhsip")
		return nil
	}

	input := &cloudformation.DeleteStackInput{
		StackName: aws.String(stack.Name),
	}

	if _, err := r.cf.DeleteStack(input); err != nil {
		return err
	}

	return r.waitWhile(stack, cloudformation.StackStatusDeleteInProgress)
}

func (r *StackReconciler) getStack(stack *cloudformationv1alpha1.Stack) (*cloudformation.Stack, error) {
	resp, err := r.cf.DescribeStacks(&cloudformation.DescribeStacksInput{
		StackName: aws.String(stack.Name),
	})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil, ErrStackNotFound
		}
		return nil, err
	}
	if len(resp.Stacks) != 1 {
		return nil, ErrStackNotFound
	}

	return resp.Stacks[0], nil
}

func (r *StackReconciler) stackExists(stack *cloudformationv1alpha1.Stack) (bool, error) {
	_, err := r.getStack(stack)
	if err != nil {
		if err == ErrStackNotFound {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func (r *StackReconciler) finalizeStacks(log logr.Logger, stack *cloudformationv1alpha1.Stack) error {
	return nil
}

func (r *StackReconciler) addFinalizer(log logr.Logger, m *cloudformationv1alpha1.Stack) error {
	return nil
}

func (r *StackReconciler) getObjectReference(owner metav1.Object) (types.UID, error) {
	ro, ok := owner.(runtime.Object)
	if !ok {
		return "", fmt.Errorf("%T is not a runtime.Object, cannot call SetControllerReference", owner)
	}

	gvk, err := apiutil.GVKForObject(ro, r.Scheme)
	if err != nil {
		return "", err
	}

	ref := *metav1.NewControllerRef(owner, schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind})
	return ref.UID, nil
}

// stackTags converts the tags field on a Stack resource to CloudFormation Tags.
// Furthermore, it adds a tag for marking ownership as well as any tags given by defaultTags.
func (r *StackReconciler) stackTags(stack *cloudformationv1alpha1.Stack) ([]*cloudformation.Tag, error) {
	ref, err := r.getObjectReference(stack)
	if err != nil {
		return nil, err
	}

	// ownership tags
	tags := []*cloudformation.Tag{
		{
			Key:   aws.String(controllerKey),
			Value: aws.String(controllerValue),
		},
		{
			Key:   aws.String(ownerKey),
			Value: aws.String(string(ref)),
		},
	}

	// default tags
	for k, v := range r.defaultTags {
		tags = append(tags, &cloudformation.Tag{
			Key:   aws.String(k),
			Value: aws.String(v),
		})
	}

	// tags specified on the Stack resource
	for k, v := range stack.Spec.Tags {
		tags = append(tags, &cloudformation.Tag{
			Key:   aws.String(k),
			Value: aws.String(v),
		})
	}

	return tags, nil
}

func (r *StackReconciler) hasOwnership(stack *cloudformationv1alpha1.Stack) (bool, error) {
	exists, err := r.stackExists(stack)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}

	cfs, err := r.getStack(stack)
	if err != nil {
		return false, err
	}

	for _, tag := range cfs.Tags {
		if aws.StringValue(tag.Key) == controllerKey && aws.StringValue(tag.Value) == controllerValue {
			return true, nil
		}
	}

	return false, nil
}

func (r *StackReconciler) updateStackStatus(stack *cloudformationv1alpha1.Stack) error {
	cfs, err := r.getStack(stack)
	if err != nil {
		return err
	}

	stackID := aws.StringValue(cfs.StackId)
	outputs := map[string]string{}
	for _, output := range cfs.Outputs {
		outputs[aws.StringValue(output.OutputKey)] = aws.StringValue(output.OutputValue)
	}

	if stackID != stack.Status.StackID || !reflect.DeepEqual(outputs, stack.Status.Outputs) {
		stack.Status.StackID = stackID
		stack.Status.Outputs = outputs

		err := r.Client.Status().Update(context.TODO(), stack)
		if err != nil {
			if errors.IsNotFound(err) {
				// Request object not found, could have been deleted after reconcile request.
				// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
				// Return and don't requeue
				// return reconcile.Result{}, nil
				return nil
			}
			// Error reading the object - requeue the request.
			// return reconcile.Result{}, err
			return err
		}
	}

	return nil
}

func (r *StackReconciler) waitWhile(stack *cloudformationv1alpha1.Stack, status string) error {
	for {
		cfs, err := r.getStack(stack)
		if err != nil {
			if err == ErrStackNotFound {
				return nil
			}
			return err
		}
		current := aws.StringValue(cfs.StackStatus)

		logrus.WithFields(logrus.Fields{
			"stack":  stack.Name,
			"status": current,
		}).Debug("waiting for stack")

		if current == status {
			time.Sleep(time.Second)
			continue
		}

		return nil
	}
}

func (r *StackReconciler) stackParameters(stack *cloudformationv1alpha1.Stack) []*cloudformation.Parameter {
	params := []*cloudformation.Parameter{}
	for k, v := range stack.Spec.Parameters {
		params = append(params, &cloudformation.Parameter{
			ParameterKey:   aws.String(k),
			ParameterValue: aws.String(v),
		})
	}
	return params
}

// SetupWithManager sets up the controller with the Manager.
func (r *StackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.configureAWSSession()
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudformationv1alpha1.Stack{}).
		Owns(&cloudformationv1alpha1.Stack{}).
		Complete(r)
}

func (r *StackReconciler) configureAWSSession() {

	assumeRole, err := StackFlagSet.GetString("assume-role")
	if err != nil {
		r.Log.Error(err, "error parsing flag")
		os.Exit(1)
	}

	region, err := StackFlagSet.GetString("region")
	if err != nil {
		r.Log.Error(err, "error parsing flag")
		os.Exit(1)
	}

	defaultTags, err := StackFlagSet.GetStringToString("tag")
	if err != nil {
		r.Log.Error(err, "error parsing flag")
		os.Exit(1)
	}

	defaultCapabilities, err := StackFlagSet.GetStringSlice("capability")
	if err != nil {
		r.Log.Error(err, "error parsing flag")
		os.Exit(1)
	}

	dryRun, err := StackFlagSet.GetBool("dry-run")
	if err != nil {
		r.Log.Error(err, "error parsing flag")
		os.Exit(1)
	}

	var client cloudformationiface.CloudFormationAPI
	sess := session.Must(session.NewSession())
	r.Log.Info(assumeRole)
	if assumeRole != "" {
		r.Log.Info("run assume")
		creds := stscreds.NewCredentials(sess, assumeRole)
		client = cloudformation.New(sess, &aws.Config{
			Credentials: creds,
			Region:      aws.String(region),
		})
	} else {
		client = cloudformation.New(sess, &aws.Config{
			Region: aws.String(region),
		})
	}
	r.cf = client
	r.defaultTags = defaultTags
	r.defaultCapabilities = defaultCapabilities
	r.dryRun = dryRun
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	for i, v := range list {
		if v == s {
			list = append(list[:i], list[i+1:]...)
		}
	}
	return list
}
