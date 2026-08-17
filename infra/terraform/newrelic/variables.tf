# ==============================================================================
# New Relic — Provider Credentials
# ==============================================================================

variable "newrelic_account_id" {
  description = "New Relic account ID that owns the LiveCart custom events (LiveCommerce*) and the dashboard created by this module."
  type        = number
  default     = 8291202
}

variable "newrelic_api_key" {
  description = "New Relic User API key (NerdGraph), used by the provider to manage the dashboard via Terraform. Never hardcode — set via TF_VAR_newrelic_api_key."
  type        = string
  sensitive   = true
}

# ==============================================================================
# Dashboard configuration
# ==============================================================================

variable "environment" {
  description = "Default selection for the dashboard's 'environment' variable (staging/production) when the dashboard first loads — every LiveCommerce* custom event now carries an `environment` attribute (exporter/config.go's Config.Environment), since staging and production share this account+dashboard."
  type        = string
  default     = "staging"

  validation {
    condition     = contains(["staging", "production"], var.environment)
    error_message = "environment must be one of: staging, production."
  }
}

variable "project_name" {
  description = "Project name used for resource naming, matching the convention in infra/terraform/variables.tf."
  type        = string
  default     = "livecart"
}

variable "dashboard_permissions" {
  description = "Who can see the dashboard in the account. One of private, public_read_only, public_read_write."
  type        = string
  default     = "public_read_only"
}

# ==============================================================================
# Documentation-only — see main.tf header comment for why this is not a real
# Terraform resource.
# ==============================================================================

variable "data_retention_days" {
  description = "DOCUMENTATION ONLY. New Relic custom event (LiveCommerceEvent/Cart/CartItem/Payment/Comment) retention is controlled at the account/plan level in New Relic, not per-dashboard via Terraform. This variable exists to record the expected retention window (30 days) for operators; it is not wired to any newrelic_* resource."
  type        = number
  default     = 30
}
