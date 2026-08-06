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

variable "raw_bucket_name" {
  description = "Globally-unique S3 bucket name for raw event archives"
  type        = string
}

variable "usage_plan_burst_limit" {
  description = "API Gateway usage plan burst (concurrent request) limit per key"
  type        = number
  default     = 10
}

variable "usage_plan_rate_limit" {
  description = "API Gateway usage plan sustained request rate limit per key (req/s)"
  type        = number
  default     = 5
}

variable "usage_plan_quota" {
  description = "API Gateway usage plan daily request quota per key"
  type        = number
  default     = 1000
}

variable "processor_schedule" {
  description = "EventBridge schedule expression for the processor Lambda"
  type        = string
  default     = "rate(5 minutes)"
}

variable "ai_enabled" {
  description = "Enable AI summarization in the processor (opt-in)"
  type        = bool
  default     = false
}

variable "ai_api_key_ssm_path" {
  description = "SSM parameter path holding the AI API key (leave empty to disable AI)"
  type        = string
  default     = ""
}

variable "ai_endpoint" {
  description = "OpenAI-compatible chat completions endpoint for AI summaries"
  type        = string
  default     = ""
}

variable "ai_model" {
  description = "AI model name for summaries"
  type        = string
  default     = ""
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
