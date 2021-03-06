package azurerm

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/keyvault/mgmt/2018-02-14/keyvault"
	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/hashicorp/terraform/helper/validation"
	uuid "github.com/satori/go.uuid"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/azure"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/response"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/set"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/tf"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/utils"
)

// As can be seen in the API definition, the Sku Family only supports the value
// `A` and is a required field
// https://github.com/Azure/azure-rest-api-specs/blob/master/arm-keyvault/2015-06-01/swagger/keyvault.json#L239
var armKeyVaultSkuFamily = "A"

var keyVaultResourceName = "azurerm_key_vault"

func resourceArmKeyVault() *schema.Resource {
	return &schema.Resource{
		Create: resourceArmKeyVaultCreateUpdate,
		Read:   resourceArmKeyVaultRead,
		Update: resourceArmKeyVaultCreateUpdate,
		Delete: resourceArmKeyVaultDelete,

		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		MigrateState:  resourceAzureRMKeyVaultMigrateState,
		SchemaVersion: 1,

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validateKeyVaultName,
			},

			"location": locationSchema(),

			"resource_group_name": resourceGroupNameSchema(),

			"sku": {
				Type:     schema.TypeList,
				Required: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:     schema.TypeString,
							Required: true,
							ValidateFunc: validation.StringInSlice([]string{
								string(keyvault.Standard),
								string(keyvault.Premium),
							}, false),
						},
					},
				},
			},

			"vault_uri": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"tenant_id": {
				Type:         schema.TypeString,
				Required:     true,
				ValidateFunc: validateUUID,
			},

			"access_policy": {
				Type:     schema.TypeList,
				Optional: true,
				Computed: true,
				MaxItems: 16,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"tenant_id": {
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: validateUUID,
						},
						"object_id": {
							Type:         schema.TypeString,
							Required:     true,
							ValidateFunc: validateUUID,
						},
						"application_id": {
							Type:         schema.TypeString,
							Optional:     true,
							ValidateFunc: validateUUID,
						},
						"certificate_permissions": azure.SchemaKeyVaultCertificatePermissions(),
						"key_permissions":         azure.SchemaKeyVaultKeyPermissions(),
						"secret_permissions":      azure.SchemaKeyVaultSecretPermissions(),
					},
				},
			},

			"enabled_for_deployment": {
				Type:     schema.TypeBool,
				Optional: true,
			},

			"enabled_for_disk_encryption": {
				Type:     schema.TypeBool,
				Optional: true,
			},

			"enabled_for_template_deployment": {
				Type:     schema.TypeBool,
				Optional: true,
			},

			"network_acls": {
				Type:     schema.TypeList,
				Optional: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"default_action": {
							Type:     schema.TypeString,
							Required: true,
							ValidateFunc: validation.StringInSlice([]string{
								string(keyvault.Allow),
								string(keyvault.Deny),
							}, false),
						},
						"bypass": {
							Type:     schema.TypeString,
							Required: true,
							ValidateFunc: validation.StringInSlice([]string{
								string(keyvault.None),
								string(keyvault.AzureServices),
							}, false),
						},
						"ip_rules": {
							Type:     schema.TypeSet,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
							Set:      schema.HashString,
						},
						"virtual_network_subnet_ids": {
							Type:     schema.TypeSet,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
							Set:      set.HashStringIgnoreCase,
						},
					},
				},
			},

			"tags": tagsSchema(),
		},
	}
}

func resourceArmKeyVaultCreateUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ArmClient).keyVaultClient
	ctx := meta.(*ArmClient).StopContext
	log.Printf("[INFO] preparing arguments for Azure ARM KeyVault creation.")

	name := d.Get("name").(string)
	resourceGroup := d.Get("resource_group_name").(string)

	if requireResourcesToBeImported && d.IsNewResource() {
		existing, err := client.Get(ctx, resourceGroup, name)
		if err != nil {
			if !utils.ResponseWasNotFound(existing.Response) {
				return fmt.Errorf("Error checking for presence of existing Key Vault %q (Resource Group %q): %s", name, resourceGroup, err)
			}
		}

		if existing.ID != nil && *existing.ID != "" {
			return tf.ImportAsExistsError("azurerm_key_vault", *existing.ID)
		}
	}

	location := azureRMNormalizeLocation(d.Get("location").(string))
	tenantUUID := uuid.FromStringOrNil(d.Get("tenant_id").(string))
	enabledForDeployment := d.Get("enabled_for_deployment").(bool)
	enabledForDiskEncryption := d.Get("enabled_for_disk_encryption").(bool)
	enabledForTemplateDeployment := d.Get("enabled_for_template_deployment").(bool)
	tags := d.Get("tags").(map[string]interface{})

	networkAclsRaw := d.Get("network_acls").([]interface{})
	networkAcls, subnetIds := expandKeyVaultNetworkAcls(networkAclsRaw)

	policies := d.Get("access_policy").([]interface{})
	accessPolicies, err := azure.ExpandKeyVaultAccessPolicies(policies)
	if err != nil {
		return fmt.Errorf("Error expanding `access_policy`: %+v", policies)
	}

	parameters := keyvault.VaultCreateOrUpdateParameters{
		Location: &location,
		Properties: &keyvault.VaultProperties{
			TenantID:                     &tenantUUID,
			Sku:                          expandKeyVaultSku(d),
			AccessPolicies:               accessPolicies,
			EnabledForDeployment:         &enabledForDeployment,
			EnabledForDiskEncryption:     &enabledForDiskEncryption,
			EnabledForTemplateDeployment: &enabledForTemplateDeployment,
			NetworkAcls:                  networkAcls,
		},
		Tags: expandTags(tags),
	}

	// Locking this resource so we don't make modifications to it at the same time if there is a
	// key vault access policy trying to update it as well
	azureRMLockByName(name, keyVaultResourceName)
	defer azureRMUnlockByName(name, keyVaultResourceName)

	// also lock on the Virtual Network ID's since modifications in the networking stack are exclusive
	virtualNetworkNames := make([]string, 0)
	for _, v := range subnetIds {
		id, err2 := parseAzureResourceID(v)
		if err2 != nil {
			return err2
		}

		virtualNetworkName := id.Path["virtualNetworks"]
		if !sliceContainsValue(virtualNetworkNames, virtualNetworkName) {
			virtualNetworkNames = append(virtualNetworkNames, virtualNetworkName)
		}
	}

	azureRMLockMultipleByName(&virtualNetworkNames, virtualNetworkResourceName)
	defer azureRMUnlockMultipleByName(&virtualNetworkNames, virtualNetworkResourceName)

	if _, err = client.CreateOrUpdate(ctx, resourceGroup, name, parameters); err != nil {
		return fmt.Errorf("Error updating Key Vault %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	read, err := client.Get(ctx, resourceGroup, name)
	if err != nil {
		return fmt.Errorf("Error retrieving Key Vault %q (Resource Group %q): %+v", name, resourceGroup, err)
	}
	if read.ID == nil {
		return fmt.Errorf("Cannot read KeyVault %s (resource Group %q) ID", name, resourceGroup)
	}

	d.SetId(*read.ID)

	if d.IsNewResource() {
		if props := read.Properties; props != nil {
			if vault := props.VaultURI; vault != nil {
				log.Printf("[DEBUG] Waiting for Key Vault %q (Resource Group %q) to become available", name, resourceGroup)
				stateConf := &resource.StateChangeConf{
					Pending:                   []string{"pending"},
					Target:                    []string{"available"},
					Refresh:                   keyVaultRefreshFunc(*vault),
					Timeout:                   30 * time.Minute,
					Delay:                     30 * time.Second,
					PollInterval:              10 * time.Second,
					ContinuousTargetOccurence: 10,
				}

				if _, err := stateConf.WaitForState(); err != nil {
					return fmt.Errorf("Error waiting for Key Vault %q (Resource Group %q) to become available: %s", name, resourceGroup, err)
				}
			}
		}
	}

	return resourceArmKeyVaultRead(d, meta)
}

func resourceArmKeyVaultRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ArmClient).keyVaultClient
	ctx := meta.(*ArmClient).StopContext

	id, err := parseAzureResourceID(d.Id())
	if err != nil {
		return err
	}
	resourceGroup := id.ResourceGroup
	name := id.Path["vaults"]

	resp, err := client.Get(ctx, resourceGroup, name)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			log.Printf("[DEBUG] Key Vault %q was not found in Resource Group %q - removing from state!", name, resourceGroup)
			d.SetId("")
			return nil
		}
		return fmt.Errorf("Error making Read request on KeyVault %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	d.Set("name", resp.Name)
	d.Set("resource_group_name", resourceGroup)
	if location := resp.Location; location != nil {
		d.Set("location", azureRMNormalizeLocation(*location))
	}

	if props := resp.Properties; props != nil {
		d.Set("tenant_id", props.TenantID.String())
		d.Set("enabled_for_deployment", props.EnabledForDeployment)
		d.Set("enabled_for_disk_encryption", props.EnabledForDiskEncryption)
		d.Set("enabled_for_template_deployment", props.EnabledForTemplateDeployment)
		d.Set("vault_uri", props.VaultURI)

		if err := d.Set("sku", flattenKeyVaultSku(props.Sku)); err != nil {
			return fmt.Errorf("Error setting `sku` for KeyVault %q: %+v", *resp.Name, err)
		}

		if err := d.Set("network_acls", flattenKeyVaultNetworkAcls(props.NetworkAcls)); err != nil {
			return fmt.Errorf("Error setting `network_acls` for KeyVault %q: %+v", *resp.Name, err)
		}

		flattenedPolicies := azure.FlattenKeyVaultAccessPolicies(props.AccessPolicies)
		if err := d.Set("access_policy", flattenedPolicies); err != nil {
			return fmt.Errorf("Error setting `access_policy` for KeyVault %q: %+v", *resp.Name, err)
		}
	}

	flattenAndSetTags(d, resp.Tags)
	return nil
}

func resourceArmKeyVaultDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ArmClient).keyVaultClient
	ctx := meta.(*ArmClient).StopContext

	id, err := parseAzureResourceID(d.Id())
	if err != nil {
		return err
	}
	resourceGroup := id.ResourceGroup
	name := id.Path["vaults"]

	azureRMLockByName(name, keyVaultResourceName)
	defer azureRMUnlockByName(name, keyVaultResourceName)

	read, err := client.Get(ctx, resourceGroup, name)
	if err != nil {
		if utils.ResponseWasNotFound(read.Response) {
			return nil
		}

		return fmt.Errorf("Error retrieving Key Vault %q (Resource Group %q): %+v", name, resourceGroup, err)
	}

	// ensure we lock on the latest network names, to ensure we handle Azure's networking layer being limited to one change at a time
	virtualNetworkNames := make([]string, 0)
	if props := read.Properties; props != nil {
		if acls := props.NetworkAcls; acls != nil {
			if rules := acls.VirtualNetworkRules; rules != nil {
				for _, v := range *rules {
					if v.ID == nil {
						continue
					}

					id, err2 := parseAzureResourceID(*v.ID)
					if err2 != nil {
						return err2
					}

					virtualNetworkName := id.Path["virtualNetworks"]
					if !sliceContainsValue(virtualNetworkNames, virtualNetworkName) {
						virtualNetworkNames = append(virtualNetworkNames, virtualNetworkName)
					}
				}
			}
		}
	}

	azureRMLockMultipleByName(&virtualNetworkNames, virtualNetworkResourceName)
	defer azureRMUnlockMultipleByName(&virtualNetworkNames, virtualNetworkResourceName)

	resp, err := client.Delete(ctx, resourceGroup, name)
	if err != nil {
		if !response.WasNotFound(resp.Response) {
			return fmt.Errorf("Error retrieving Key Vault %q (Resource Group %q): %+v", name, resourceGroup, err)
		}
	}

	return nil
}

func expandKeyVaultSku(d *schema.ResourceData) *keyvault.Sku {
	skuSets := d.Get("sku").([]interface{})
	sku := skuSets[0].(map[string]interface{})

	return &keyvault.Sku{
		Family: &armKeyVaultSkuFamily,
		Name:   keyvault.SkuName(sku["name"].(string)),
	}
}

func flattenKeyVaultSku(sku *keyvault.Sku) []interface{} {
	result := map[string]interface{}{
		"name": string(sku.Name),
	}

	return []interface{}{result}
}

func flattenKeyVaultNetworkAcls(input *keyvault.NetworkRuleSet) []interface{} {
	if input == nil {
		return []interface{}{}
	}

	output := make(map[string]interface{})

	output["bypass"] = string(input.Bypass)
	output["default_action"] = string(input.DefaultAction)

	ipRules := make([]interface{}, 0)
	if input.IPRules != nil {
		for _, v := range *input.IPRules {
			if v.Value == nil {
				continue
			}

			ipRules = append(ipRules, *v.Value)
		}
	}
	output["ip_rules"] = schema.NewSet(schema.HashString, ipRules)

	virtualNetworkRules := make([]interface{}, 0)
	if input.VirtualNetworkRules != nil {
		for _, v := range *input.VirtualNetworkRules {
			if v.ID == nil {
				continue
			}

			virtualNetworkRules = append(virtualNetworkRules, *v.ID)
		}
	}
	output["virtual_network_subnet_ids"] = schema.NewSet(schema.HashString, virtualNetworkRules)

	return []interface{}{output}
}

func validateKeyVaultName(v interface{}, k string) (warnings []string, errors []error) {
	value := v.(string)
	if matched := regexp.MustCompile(`^[a-zA-Z0-9-]{3,24}$`).Match([]byte(value)); !matched {
		errors = append(errors, fmt.Errorf("%q may only contain alphanumeric characters and dashes and must be between 3-24 chars", k))
	}

	return warnings, errors
}

func keyVaultRefreshFunc(vaultUri string) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		log.Printf("[DEBUG] Checking to see if KeyVault %q is available..", vaultUri)

		var PTransport = &http.Transport{Proxy: http.ProxyFromEnvironment}

		client := &http.Client{
			Transport: PTransport,
		}

		conn, err := client.Get(vaultUri)
		if err != nil {
			log.Printf("[DEBUG] Didn't find KeyVault at %q", vaultUri)
			return nil, "pending", fmt.Errorf("Error connecting to %q: %s", vaultUri, err)
		}

		defer conn.Body.Close()

		log.Printf("[DEBUG] Found KeyVault at %q", vaultUri)
		return "available", "available", nil
	}
}

func expandKeyVaultNetworkAcls(input []interface{}) (*keyvault.NetworkRuleSet, []string) {
	subnetIds := make([]string, 0)
	if len(input) == 0 {
		return nil, subnetIds
	}

	v := input[0].(map[string]interface{})

	bypass := v["bypass"].(string)
	defaultAction := v["default_action"].(string)

	ipRulesRaw := v["ip_rules"].(*schema.Set)
	ipRules := make([]keyvault.IPRule, 0)

	for _, v := range ipRulesRaw.List() {
		rule := keyvault.IPRule{
			Value: utils.String(v.(string)),
		}
		ipRules = append(ipRules, rule)
	}

	networkRulesRaw := v["virtual_network_subnet_ids"].(*schema.Set)
	networkRules := make([]keyvault.VirtualNetworkRule, 0)
	for _, v := range networkRulesRaw.List() {
		rawId := v.(string)
		subnetIds = append(subnetIds, rawId)
		rule := keyvault.VirtualNetworkRule{
			ID: utils.String(rawId),
		}
		networkRules = append(networkRules, rule)
	}

	ruleSet := keyvault.NetworkRuleSet{
		Bypass:              keyvault.NetworkRuleBypassOptions(bypass),
		DefaultAction:       keyvault.NetworkRuleAction(defaultAction),
		IPRules:             &ipRules,
		VirtualNetworkRules: &networkRules,
	}
	return &ruleSet, subnetIds
}
