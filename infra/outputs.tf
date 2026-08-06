output "api_gateway_url" {
  description = "Base URL of the ingest API Gateway"
  value       = aws_api_gateway_stage.main.invoke_url
}

output "envelope_endpoint" {
  description = "Envelope ingest endpoint (Sentry SDK targets this with a DSN change)"
  value       = "${aws_api_gateway_stage.main.invoke_url}/api/{project_id}/envelope/"
}

output "store_endpoint" {
  description = "Store ingest endpoint (legacy Sentry SDKs)"
  value       = "${aws_api_gateway_stage.main.invoke_url}/api/{project_id}/store/"
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

output "usage_plan_id" {
  description = "API Gateway usage plan id (throttle/quota control)"
  value       = aws_api_gateway_usage_plan.main.id
}

output "api_key_id" {
  description = "API Gateway API key id for keyed clients"
  value       = aws_api_gateway_api_key.main.id
}

output "ingest_function_name" {
  description = "Ingest Lambda function name"
  value       = aws_lambda_function.main["ingest"].function_name
}

output "query_function_name" {
  description = "Query Lambda function name"
  value       = aws_lambda_function.main["query"].function_name
}
