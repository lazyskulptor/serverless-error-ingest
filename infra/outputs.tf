output "api_gateway_url" {
  description = "Base URL of the ingest API Gateway"
  value       = aws_api_gateway_stage.main.invoke_url
}

output "custom_domain_url" {
  description = "Custom-domain base URL, or null when custom DNS is disabled"
  value       = var.domain_name != "" ? "https://${var.domain_name}" : null
}

output "public_base_url" {
  description = "Effective public base URL used by clients and DSNs"
  value       = var.domain_name != "" ? "https://${var.domain_name}" : aws_api_gateway_stage.main.invoke_url
}

output "envelope_endpoint" {
  description = "Envelope ingest endpoint (Sentry SDK targets this with a DSN change)"
  value       = "${var.domain_name != "" ? "https://${var.domain_name}" : aws_api_gateway_stage.main.invoke_url}/api/{project_id}/envelope/"
}

output "store_endpoint" {
  description = "Store ingest endpoint (legacy Sentry SDKs)"
  value       = "${var.domain_name != "" ? "https://${var.domain_name}" : aws_api_gateway_stage.main.invoke_url}/api/{project_id}/store/"
}

output "raw_bucket" {
  description = "S3 bucket storing raw envelopes/events"
  value       = aws_s3_bucket.raw.id
}

output "projects_table" {
  description = "DynamoDB table holding DSN public-key registry"
  value       = aws_dynamodb_table.projects.name
}

output "events_table" {
  description = "DynamoDB table holding event metadata index"
  value       = aws_dynamodb_table.events.name
}

output "ingest_function_name" {
  description = "Ingest Lambda function name"
  value       = aws_lambda_function.main["ingest"].function_name
}
