variable "aws_region" {
  type    = string
  default = "ap-northeast-2"
}

variable "github_repository" {
  type    = string
  default = "lazyskulptor/serverless-error-ingest"
}

variable "github_environment" {
  type    = string
  default = "production"
}

variable "state_bucket" {
  type = string
}

variable "state_key" {
  type    = string
  default = "serverless-error-ingest/production/terraform.tfstate"
}

variable "state_lock_table" {
  type = string
}

variable "resource_name_prefix" {
  type    = string
  default = "sentry-ingest"
}

variable "raw_bucket_name" {
  type = string
}

variable "route53_zone_arn" {
  description = "Optional hosted-zone ARN used by production"
  type        = string
  default     = ""
}
