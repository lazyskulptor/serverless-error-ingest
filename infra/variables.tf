variable "region" {
  description = "AWS region for all resources"
  type        = string
  default     = "ap-northeast-2"
}

variable "name_prefix" {
  description = "Prefix for all resource names"
  type        = string
  default     = "sentry-ingest"
}

variable "environment" {
  description = "Deployment environment tag (dev/staging/prod)"
  type        = string
  default     = "dev"
}

variable "stage" {
  description = "API Gateway stage name"
  type        = string
  default     = "v1"
}

variable "domain_name" {
  description = "Optional custom API hostname; leave empty to use the API Gateway invoke URL"
  type        = string
  default     = ""

  validation {
    condition     = var.domain_name == "" || can(regex("^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$", var.domain_name))
    error_message = "domain_name must be empty or a lower-case DNS hostname."
  }
}

variable "dns_provider" {
  description = "DNS provider used for ACM validation and the API hostname: aws or cloudflare"
  type        = string
  default     = "aws"

  validation {
    condition     = contains(["aws", "cloudflare"], var.dns_provider)
    error_message = "dns_provider must be either aws or cloudflare."
  }
}

variable "route53_zone_id" {
  description = "Existing Route 53 hosted zone ID; required for an AWS-managed custom domain"
  type        = string
  default     = ""
}

variable "cloudflare_zone_id" {
  description = "Existing Cloudflare zone ID; required for a Cloudflare-managed custom domain"
  type        = string
  default     = ""
}

variable "cloudflare_proxied" {
  description = "Proxy the API hostname through Cloudflare; DNS-only false is the supported default"
  type        = bool
  default     = false

  validation {
    condition     = !var.cloudflare_proxied || var.dns_provider == "cloudflare"
    error_message = "cloudflare_proxied may be true only when dns_provider is cloudflare."
  }
}

variable "raw_bucket_name" {
  description = "Globally-unique S3 bucket name for raw event archives"
  type        = string
}

variable "allow_destroy_data" {
  description = "Allow destroy to permanently delete S3 objects/versions and DynamoDB tables. Defaults to true for disposable development deployments; production must set false before first apply."
  type        = bool
  default     = true
}

variable "waf_rate_limit" {
  description = "WAF rate-based rule limit: max requests per 5 minutes per source IP"
  type        = number
  default     = 2000
}

variable "raw_standard_ia_days" {
  description = "Days before raw S3 objects transition to STANDARD_IA"
  type        = number
  default     = 30
}

variable "raw_expiration_days" {
  description = "Days before raw S3 objects expire"
  type        = number
  default     = 180
}

variable "event_ttl_days" {
  description = "Days before DynamoDB event metadata rows expire (TTL)"
  type        = number
  default     = 90
}

variable "alarm_sns_topic_arn" {
  description = "Optional existing SNS topic ARN for alarm and recovery notifications"
  type        = string
  default     = ""

  validation {
    condition     = var.alarm_sns_topic_arn == "" || can(regex("^arn:aws[a-z-]*:sns:", var.alarm_sns_topic_arn))
    error_message = "alarm_sns_topic_arn must be empty or an SNS topic ARN."
  }
}

variable "s3_object_count_alarm_threshold" {
  description = "Alarm when the daily S3 NumberOfObjects metric reaches this count"
  type        = number
  default     = 100000
}

variable "s3_bucket_size_alarm_bytes" {
  description = "Alarm when the daily S3 StandardStorage BucketSizeBytes metric reaches this size"
  type        = number
  default     = 5368709120
}

variable "api_hourly_request_alarm_threshold" {
  description = "Alarm when API Gateway receives this many requests in one hour"
  type        = number
  default     = 10000
}
