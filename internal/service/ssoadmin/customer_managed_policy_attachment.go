package ssoadmin

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ssoadmin"
	"github.com/hashicorp/aws-sdk-go-base/v2/awsv1shim/v2/tfawserr"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/tfresource"
	"github.com/hashicorp/terraform-provider-aws/internal/verify"
)

const (
	customerPolicyAttachmentTimeout = 5 * time.Minute
)

func ResourceCustomerManagedPolicyAttachment() *schema.Resource {
	return &schema.Resource{
		Create: resourceCustomerManagedPolicyAttachmentCreate,
		Read:   resourceCustomerManagedPolicyAttachmentRead,
		Delete: resourceCustomerManagedPolicyAttachmentDelete,

		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		Schema: map[string]*schema.Schema{
			"customer_managed_policy_reference": {
				Type:     schema.TypeList,
				Required: true,
				ForceNew: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:         schema.TypeString,
							Required:     true,
							ForceNew:     true,
							ValidateFunc: validation.StringLenBetween(0, 128),
						},
						"path": {
							Type:         schema.TypeString,
							Optional:     true,
							Default:      "/",
							ForceNew:     true,
							ValidateFunc: validation.StringLenBetween(0, 512),
						},
					},
				},
			},
			"instance_arn": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: verify.ValidARN,
			},
			"permission_set_arn": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: verify.ValidARN,
			},
		},
	}
}

func resourceCustomerManagedPolicyAttachmentCreate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*conns.AWSClient).SSOAdminConn

	tfMap := d.Get("customer_managed_policy_reference").([]interface{})[0].(map[string]interface{})
	policyName := tfMap["name"].(string)
	policyPath := tfMap["path"].(string)
	instanceARN := d.Get("instance_arn").(string)
	permissionSetARN := d.Get("permission_set_arn").(string)
	id := CustomerManagedPolicyAttachmentCreateResourceID(policyName, policyPath, permissionSetARN, instanceARN)
	input := &ssoadmin.AttachCustomerManagedPolicyReferenceToPermissionSetInput{
		CustomerManagedPolicyReference: expandCustomerManagedPolicyReference(tfMap),
		InstanceArn:                    aws.String(instanceARN),
		PermissionSetArn:               aws.String(permissionSetARN),
	}

	log.Printf("[INFO] Attaching customer managed policy reference to permission set: %s", input)
	_, err := tfresource.RetryWhenAWSErrCodeEquals(customerPolicyAttachmentTimeout, func() (interface{}, error) {
		return conn.AttachCustomerManagedPolicyReferenceToPermissionSet(input)
	}, ssoadmin.ErrCodeConflictException, ssoadmin.ErrCodeThrottlingException)

	if err != nil {
		return fmt.Errorf("creating SSO Customer Managed Policy Attachment (%s): %w", id, err)
	}

	d.SetId(id)

	// After the policy has been attached to the permission set, provision in all accounts that use this permission set.
	if err := provisionPermissionSet(conn, permissionSetARN, instanceARN); err != nil {
		return err
	}

	return resourceCustomerManagedPolicyAttachmentRead(d, meta)
}

func resourceCustomerManagedPolicyAttachmentRead(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*conns.AWSClient).SSOAdminConn

	policyName, policyPath, permissionSetARN, instanceARN, err := CustomerManagedPolicyAttachmentParseResourceID(d.Id())

	if err != nil {
		return err
	}

	policy, err := FindCustomerManagedPolicy(conn, policyName, policyPath, permissionSetARN, instanceARN)

	if !d.IsNewResource() && tfawserr.ErrCodeEquals(err, ssoadmin.ErrCodeResourceNotFoundException) {
		log.Printf("[WARN] SSO Customer Managed Policy Attachment (%s) not found, removing from state", d.Id())
		d.SetId("")
		return nil
	}

	if err != nil {
		return fmt.Errorf("reading SSO Customer Managed Policy Attachment (%s): %w", d.Id(), err)
	}

	if err := d.Set("customer_managed_policy_reference", []interface{}{flattenCustomerManagedPolicyReference(policy)}); err != nil {
		return fmt.Errorf("setting customer_managed_policy_reference: %w", err)
	}
	d.Set("instance_arn", instanceARN)
	d.Set("permission_set_arn", permissionSetARN)

	return nil
}

func resourceCustomerManagedPolicyAttachmentDelete(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*conns.AWSClient).SSOAdminConn

	policyName, policyPath, permissionSetArn, instanceArn, err := CustomerManagedPolicyAttachmentParseResourceID(d.Id())
	if err != nil {
		return fmt.Errorf("error parsing SSO Customer Managed Policy Attachment ID: %w", err)
	}

	input := &ssoadmin.DetachCustomerManagedPolicyReferenceFromPermissionSetInput{
		InstanceArn:      aws.String(instanceArn),
		PermissionSetArn: aws.String(permissionSetArn),
		CustomerManagedPolicyReference: &ssoadmin.CustomerManagedPolicyReference{
			Name: aws.String(policyName),
			Path: aws.String(policyPath),
		},
	}
	// A retry might be required whilst changes propagate, particularly if updating multiple attachments
	err = resource.Retry(customerPolicyAttachmentTimeout, func() *resource.RetryError {
		var err error
		_, err = conn.DetachCustomerManagedPolicyReferenceFromPermissionSet(input)

		if err != nil {
			if tfawserr.ErrCodeEquals(err, ssoadmin.ErrCodeConflictException) {
				return resource.RetryableError(err)
			}
			if tfawserr.ErrCodeEquals(err, ssoadmin.ErrCodeThrottlingException) {
				return resource.RetryableError(err)
			}
			return resource.NonRetryableError(err)
		}
		return nil

	})

	if tfresource.TimedOut(err) {
		_, err = conn.DetachCustomerManagedPolicyReferenceFromPermissionSet(input)
	}

	if err != nil {
		if tfawserr.ErrCodeEquals(err, ssoadmin.ErrCodeResourceNotFoundException) {
			return nil
		}
		return fmt.Errorf("error detaching Customer Managed Policy (%s) from SSO Permission Set (%s): %w", policyName, permissionSetArn, err)
	}

	// After the policy has been detached from the permission set, provision in all accounts that use this permission set
	if err := provisionPermissionSet(conn, permissionSetArn, instanceArn); err != nil {
		return err
	}

	return nil
}

const customerManagedPolicyAttachmentIDSeparator = ","

func CustomerManagedPolicyAttachmentCreateResourceID(policyName, policyPath, permissionSetARN, instanceARN string) string {
	parts := []string{policyName, policyPath, permissionSetARN, instanceARN}
	id := strings.Join(parts, customerManagedPolicyAttachmentIDSeparator)

	return id
}

func CustomerManagedPolicyAttachmentParseResourceID(id string) (string, string, string, string, error) {
	parts := strings.Split(id, customerManagedPolicyAttachmentIDSeparator)

	if len(parts) == 4 && parts[0] != "" && parts[1] != "" && parts[2] != "" && parts[3] != "" {
		return parts[0], parts[1], parts[2], parts[3], nil
	}

	return "", "", "", "", fmt.Errorf("unexpected format for ID (%[1]s), expected CUSTOMER_MANAGED_POLICY_NAME%[2]sCUSTOMER_MANAGED_POLICY_PATH%[2]sPERMISSION_SET_ARN%[2]sINSTANCE_ARN", id, customerManagedPolicyAttachmentIDSeparator)
}

func expandCustomerManagedPolicyReference(tfMap map[string]interface{}) *ssoadmin.CustomerManagedPolicyReference {
	if tfMap == nil {
		return nil
	}

	apiObject := &ssoadmin.CustomerManagedPolicyReference{}

	if v, ok := tfMap["name"].(string); ok && v != "" {
		apiObject.Name = aws.String(v)
	}

	if v, ok := tfMap["path"].(string); ok && v != "" {
		apiObject.Path = aws.String(v)
	}

	return apiObject
}

func flattenCustomerManagedPolicyReference(apiObject *ssoadmin.CustomerManagedPolicyReference) map[string]interface{} {
	if apiObject == nil {
		return nil
	}

	tfMap := map[string]interface{}{}

	if v := apiObject.Name; v != nil {
		tfMap["name"] = aws.StringValue(v)
	}

	if v := apiObject.Path; v != nil {
		tfMap["path"] = aws.StringValue(v)
	}

	return tfMap
}
