# ---------------------------------------------------------------------------
# Serverless Sentry-Compatible Error Ingest — OpenTofu infrastructure
# API Gateway (REST) + Lambda (ingest/query) + S3 (raw) + DynamoDB (index)
# ---------------------------------------------------------------------------

terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }

  # Remote state (S3 + DynamoDB lock). Values are static here because backend
  # configuration cannot use variables; supply them at init time with
  # -backend-config (see infra/README.md "State management"):
  #
  #   tofu init \
  #     -backend-config="bucket=<state-bucket>" \
  #     -backend-config="key=envs/<environment>/terraform.tfstate" \
  #     -backend-config="region=<region>" \
  #     -backend-config="dynamodb_table=<lock-table>" \
  #     -backend-config="encrypt=true"
  #
  # CI runs `tofu init -backend=false` (validation only, no state access).
  backend "s3" {}
}

provider "aws" {
  region = var.region
}

locals {
  name_prefix = var.name_prefix

  common_tags = {
    Project     = "serverless-sentry-ingest"
    ManagedBy   = "opentofu"
    Environment = var.environment
  }

  # Map of route key -> { resource id, lambda key, http method }
  route_resource_ids = {
    envelope = aws_api_gateway_resource.envelope.id
    store    = aws_api_gateway_resource.store.id
    events   = aws_api_gateway_resource.events.id
  }

  api_routes = {
    envelope = { resource = "envelope", lambda = "ingest", http = "POST" }
    store    = { resource = "store", lambda = "ingest", http = "POST" }
    events   = { resource = "events", lambda = "query", http = "GET" }
  }
}

# ---------------------------------------------------------------------------
# S3 — raw event/envelope archive (private, versioned)
# ---------------------------------------------------------------------------

resource "aws_s3_bucket" "raw" {
  bucket = var.raw_bucket_name
  tags   = local.common_tags
}

resource "aws_s3_bucket_versioning" "raw" {
  bucket = aws_s3_bucket.raw.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_public_access_block" "raw" {
  bucket = aws_s3_bucket.raw.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "raw" {
  bucket = aws_s3_bucket.raw.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Cost control: tier raw archives to STANDARD_IA, then expire them.
resource "aws_s3_bucket_lifecycle_configuration" "raw" {
  bucket = aws_s3_bucket.raw.id

  rule {
    id     = "archive-tiering"
    status = "Enabled"
    filter {}

    transition {
      days          = var.raw_standard_ia_days
      storage_class = "STANDARD_IA"
    }

    expiration {
      days = var.raw_expiration_days
    }
  }
}

# ---------------------------------------------------------------------------
# DynamoDB — projects registry (DSN public keys) + events index (metadata)
# ---------------------------------------------------------------------------

resource "aws_dynamodb_table" "projects" {
  name         = "${local.name_prefix}-projects"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "project_id"

  attribute {
    name = "project_id"
    type = "S"
  }

  tags = local.common_tags
}

resource "aws_dynamodb_table" "events" {
  name         = "${local.name_prefix}-events"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "project_id"
  range_key    = "event_id"

  attribute {
    name = "project_id"
    type = "S"
  }
  attribute {
    name = "event_id"
    type = "S"
  }
  attribute {
    name = "timestamp"
    type = "S"
  }

  global_secondary_index {
    name            = "TimestampIndex"
    hash_key        = "project_id"
    range_key       = "timestamp"
    projection_type = "ALL"
  }

  # Cost control: metadata rows expire after `event_ttl_days`.
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }

  tags = local.common_tags
}

# ---------------------------------------------------------------------------
# IAM — least-privilege roles per Lambda handler
# ---------------------------------------------------------------------------

resource "aws_iam_role" "ingest" {
  name = "${local.name_prefix}-ingest-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "lambda.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = local.common_tags
}

resource "aws_iam_role_policy" "ingest" {
  name = "${local.name_prefix}-ingest-policy"
  role = aws_iam_role.ingest.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["s3:PutObject"]
        Resource = ["${aws_s3_bucket.raw.arn}/*"]
      },
      {
        Effect   = "Allow"
        Action   = ["dynamodb:PutItem"]
        Resource = [aws_dynamodb_table.events.arn]
      },
      {
        Effect   = "Allow"
        Action   = ["dynamodb:GetItem"]
        Resource = [aws_dynamodb_table.projects.arn]
      }
    ]
  })
}

resource "aws_iam_role" "query" {
  name = "${local.name_prefix}-query-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "lambda.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = local.common_tags
}

resource "aws_iam_role_policy" "query" {
  name = "${local.name_prefix}-query-policy"
  role = aws_iam_role.query.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["dynamodb:Query"]
        Resource = [aws_dynamodb_table.events.arn]
      },
      {
        Effect   = "Allow"
        Action   = ["dynamodb:GetItem"]
        Resource = [aws_dynamodb_table.projects.arn]
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = ["${aws_s3_bucket.raw.arn}/*"]
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "ingest_logs" {
  role       = aws_iam_role.ingest.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_iam_role_policy_attachment" "ingest_xray" {
  role       = aws_iam_role.ingest.name
  policy_arn = "arn:aws:iam::aws:policy/AWSXRayDaemonWriteAccess"
}

resource "aws_iam_role_policy_attachment" "query_logs" {
  role       = aws_iam_role.query.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_iam_role_policy_attachment" "query_xray" {
  role       = aws_iam_role.query.name
  policy_arn = "arn:aws:iam::aws:policy/AWSXRayDaemonWriteAccess"
}

# ---------------------------------------------------------------------------
# Lambda functions (Go, provided.al2023 + bootstrap)
# Build artifacts are produced by `make lambda` — see infra/README.md.
# ---------------------------------------------------------------------------

resource "aws_lambda_function" "main" {
  for_each = {
    ingest = {
      role        = aws_iam_role.ingest.arn
      zip_path    = "../lambda/ingest/function.zip"
      timeout     = 30
      memory_size = 256
      description = "Sentry wire-protocol ingest handler"
      env_vars = {
        RAW_BUCKET     = aws_s3_bucket.raw.id
        EVENTS_TABLE   = aws_dynamodb_table.events.name
        PROJECTS_TABLE = aws_dynamodb_table.projects.name
      }
    }
    query = {
      role        = aws_iam_role.query.arn
      zip_path    = "../lambda/query/function.zip"
      timeout     = 15
      memory_size = 128
      description = "Read-only event metadata query handler"
      env_vars = {
        EVENTS_TABLE   = aws_dynamodb_table.events.name
        PROJECTS_TABLE = aws_dynamodb_table.projects.name
        RAW_BUCKET     = aws_s3_bucket.raw.id
      }
    }
  }

  function_name = "${local.name_prefix}-${each.key}"
  role          = each.value.role
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  architectures = ["arm64"]
  timeout       = each.value.timeout
  memory_size   = each.value.memory_size
  description   = each.value.description

  filename = each.value.zip_path

  environment {
    variables = each.value.env_vars
  }

  tracing_config {
    mode = "Active"
  }

  tags = local.common_tags
}

resource "aws_lambda_permission" "api_gateway" {
  for_each = local.api_routes

  statement_id  = "AllowExecutionFromAPIGateway-${each.key}"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.main[each.value.lambda].function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.main.execution_arn}/*/${each.value.http}/${each.key}"
}

# ---------------------------------------------------------------------------
# Processor Lambda (post-ingest grouping/summarization, scheduled)
# ---------------------------------------------------------------------------

resource "aws_iam_role" "processor" {
  name = "${local.name_prefix}-processor-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "lambda.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = local.common_tags
}

resource "aws_iam_role_policy" "processor" {
  name = "${local.name_prefix}-processor-policy"
  role = aws_iam_role.processor.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["dynamodb:Scan", "dynamodb:UpdateItem"]
        Resource = [aws_dynamodb_table.events.arn]
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = ["${aws_s3_bucket.raw.arn}/*"]
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "processor_logs" {
  role       = aws_iam_role.processor.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_iam_role_policy_attachment" "processor_xray" {
  role       = aws_iam_role.processor.name
  policy_arn = "arn:aws:iam::aws:policy/AWSXRayDaemonWriteAccess"
}

# Optional AI key from SSM — never in source control.
data "aws_ssm_parameter" "ai_key" {
  count = var.ai_api_key_ssm_path != "" ? 1 : 0
  name  = var.ai_api_key_ssm_path
}

resource "aws_lambda_function" "processor" {
  function_name = "${local.name_prefix}-processor"
  role          = aws_iam_role.processor.arn
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  architectures = ["arm64"]
  timeout       = 60
  memory_size   = 256
  description   = "Post-ingest grouping/summarization processor"

  filename = "../lambda/processor/function.zip"

  tracing_config {
    mode = "Active"
  }

  environment {
    variables = merge(
      {
        EVENTS_TABLE = aws_dynamodb_table.events.name
        RAW_BUCKET   = aws_s3_bucket.raw.id
        # AI is opt-in: leave unset for deterministic grouping only.
        PROJECT_ALLOW_AI = var.ai_enabled ? "true" : "false"
      },
      var.ai_api_key_ssm_path != "" ? { AI_API_KEY = data.aws_ssm_parameter.ai_key[0].value } : {},
      var.ai_endpoint != "" ? { AI_ENDPOINT = var.ai_endpoint } : {},
      var.ai_model != "" ? { AI_MODEL = var.ai_model } : {},
    )
  }

  tags = local.common_tags
}

resource "aws_cloudwatch_event_rule" "processor" {
  name                = "${local.name_prefix}-processor-schedule"
  description         = "Scheduled processor run for grouping/summarization"
  schedule_expression = var.processor_schedule
  tags                = local.common_tags
}

resource "aws_cloudwatch_event_target" "processor" {
  rule      = aws_cloudwatch_event_rule.processor.name
  target_id = "processor"
  arn       = aws_lambda_function.processor.arn
}

resource "aws_lambda_permission" "processor_events" {
  statement_id  = "AllowExecutionFromEventBridge"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.processor.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.processor.arn
}

# ---------------------------------------------------------------------------
# WAFv2 — global rate-based abuse prevention in front of the stage
# The in-Lambda token-bucket limiter is per-execution-environment memory and
# cannot enforce a true global rate; WAF is the primary control and the Lambda
# limiter is documented defense-in-depth.
# ---------------------------------------------------------------------------

resource "aws_wafv2_web_acl" "main" {
  name        = "${local.name_prefix}-waf"
  description = "Rate-based abuse prevention for the ingest API"
  scope       = "REGIONAL"

  default_action {
    allow {}
  }

  rule {
    name     = "rate-limit"
    priority = 1

    action {
      block {}
    }

    statement {
      rate_based_statement {
        limit              = var.waf_rate_limit
        aggregate_key_type = "IP"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}RateLimit"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${local.name_prefix}WAF"
    sampled_requests_enabled   = true
  }

  tags = local.common_tags
}

resource "aws_wafv2_web_acl_association" "main" {
  resource_arn = aws_api_gateway_stage.main.arn
  web_acl_arn  = aws_wafv2_web_acl.main.arn
}

# ---------------------------------------------------------------------------
# API Gateway (REST) — /api/{project_id}/envelope, /store, /api/projects/{project_id}/events
# ---------------------------------------------------------------------------

resource "aws_api_gateway_rest_api" "main" {
  name        = "${local.name_prefix}-api"
  description = "Serverless Sentry-compatible ingest API"

  binary_media_types = [
    "application/x-sentry-envelope",
    "multipart/form-data",
    "application/octet-stream",
  ]

  tags = local.common_tags
}

# /api
resource "aws_api_gateway_resource" "api" {
  rest_api_id = aws_api_gateway_rest_api.main.id
  parent_id   = aws_api_gateway_rest_api.main.root_resource_id
  path_part   = "api"
}

# /api/{project_id}
resource "aws_api_gateway_resource" "project" {
  rest_api_id = aws_api_gateway_rest_api.main.id
  parent_id   = aws_api_gateway_resource.api.id
  path_part   = "{project_id}"
}

# /api/{project_id}/envelope
resource "aws_api_gateway_resource" "envelope" {
  rest_api_id = aws_api_gateway_rest_api.main.id
  parent_id   = aws_api_gateway_resource.project.id
  path_part   = "envelope"
}

# /api/{project_id}/store
resource "aws_api_gateway_resource" "store" {
  rest_api_id = aws_api_gateway_rest_api.main.id
  parent_id   = aws_api_gateway_resource.project.id
  path_part   = "store"
}

# /api/projects (static — distinct from {project_id})
resource "aws_api_gateway_resource" "projects_root" {
  rest_api_id = aws_api_gateway_rest_api.main.id
  parent_id   = aws_api_gateway_resource.api.id
  path_part   = "projects"
}

# /api/projects/{project_id}
resource "aws_api_gateway_resource" "project_by_id" {
  rest_api_id = aws_api_gateway_rest_api.main.id
  parent_id   = aws_api_gateway_resource.projects_root.id
  path_part   = "{project_id}"
}

# /api/projects/{project_id}/events
resource "aws_api_gateway_resource" "events" {
  rest_api_id = aws_api_gateway_rest_api.main.id
  parent_id   = aws_api_gateway_resource.project_by_id.id
  path_part   = "events"
}

# --- Lambda proxy methods (POST/GET) ---

resource "aws_api_gateway_method" "main" {
  for_each = local.api_routes

  rest_api_id      = aws_api_gateway_rest_api.main.id
  resource_id      = local.route_resource_ids[each.value.resource]
  http_method      = each.value.http
  authorization    = "NONE"
  api_key_required = false
}

resource "aws_api_gateway_method_response" "main" {
  for_each = local.api_routes

  rest_api_id = aws_api_gateway_rest_api.main.id
  resource_id = local.route_resource_ids[each.value.resource]
  http_method = each.value.http
  status_code = "200"
}

resource "aws_api_gateway_integration" "main" {
  for_each = local.api_routes

  rest_api_id             = aws_api_gateway_rest_api.main.id
  resource_id             = local.route_resource_ids[each.value.resource]
  http_method             = each.value.http
  type                    = "AWS_PROXY"
  integration_http_method = "POST"
  uri                     = aws_lambda_function.main[each.value.lambda].invoke_arn
}

# --- CORS preflight: OPTIONS mock on every route ---

resource "aws_api_gateway_method" "options" {
  for_each = local.route_resource_ids

  rest_api_id   = aws_api_gateway_rest_api.main.id
  resource_id   = each.value
  http_method   = "OPTIONS"
  authorization = "NONE"
}

resource "aws_api_gateway_method_response" "options" {
  for_each = local.route_resource_ids

  rest_api_id = aws_api_gateway_rest_api.main.id
  resource_id = each.value
  http_method = "OPTIONS"
  status_code = "200"

  response_parameters = {
    "method.response.header.Access-Control-Allow-Headers" = true
    "method.response.header.Access-Control-Allow-Methods" = true
    "method.response.header.Access-Control-Allow-Origin"  = true
  }
}

resource "aws_api_gateway_integration" "options" {
  for_each = local.route_resource_ids

  rest_api_id = aws_api_gateway_rest_api.main.id
  resource_id = each.value
  http_method = "OPTIONS"
  type        = "MOCK"

  request_templates = {
    "application/json" = "{\"statusCode\": 200}"
  }
}

resource "aws_api_gateway_integration_response" "options" {
  for_each = local.route_resource_ids

  rest_api_id = aws_api_gateway_rest_api.main.id
  resource_id = each.value
  http_method = "OPTIONS"
  status_code = "200"

  response_parameters = {
    "method.response.header.Access-Control-Allow-Headers" = "'Content-Type, X-Sentry-Auth, X-Requested-With, X-Forwarded-For, Origin, Accept, Authentication, Authorization, Content-Encoding, Transfer-Encoding'"
    "method.response.header.Access-Control-Allow-Methods" = "'POST, OPTIONS'"
    "method.response.header.Access-Control-Allow-Origin"  = "'*'"
  }
}

# --- Gateway responses: throttled/quota-exceeded must surface 429 + Retry-After ---

resource "aws_api_gateway_gateway_response" "throttled" {
  rest_api_id   = aws_api_gateway_rest_api.main.id
  response_type = "THROTTLED"
  status_code   = "429"

  response_parameters = {
    "gatewayresponse.header.Retry-After"                 = "60"
    "gatewayresponse.header.Access-Control-Allow-Origin" = "'*'"
  }

  response_templates = {
    "application/json" = "{\"detail\":\"Rate limit exceeded\"}"
  }
}

resource "aws_api_gateway_gateway_response" "quota_exceeded" {
  rest_api_id   = aws_api_gateway_rest_api.main.id
  response_type = "QUOTA_EXCEEDED"
  status_code   = "429"

  response_parameters = {
    "gatewayresponse.header.Retry-After"                 = "60"
    "gatewayresponse.header.Access-Control-Allow-Origin" = "'*'"
  }

  response_templates = {
    "application/json" = "{\"detail\":\"Request quota exceeded\"}"
  }
}

# --- Deployment, stage, usage plan ---

resource "aws_api_gateway_deployment" "main" {
  rest_api_id = aws_api_gateway_rest_api.main.id

  triggers = {
    redeployment = sha1(jsonencode({
      rest_api = aws_api_gateway_rest_api.main.id
      methods  = [for m in aws_api_gateway_method.main : m.id]
      opts     = [for m in aws_api_gateway_method.options : m.id]
      integs   = [for i in aws_api_gateway_integration.main : i.id]
      oints    = [for i in aws_api_gateway_integration.options : i.id]
    }))
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_api_gateway_stage" "main" {
  stage_name    = var.stage
  rest_api_id   = aws_api_gateway_rest_api.main.id
  deployment_id = aws_api_gateway_deployment.main.id

  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.api_gw.arn
    format = jsonencode({
      requestId        = "$context.requestId"
      requestTime      = "$context.requestTime"
      routeKey         = "$context.resourcePath"
      method           = "$context.httpMethod"
      status           = "$context.status"
      protocol         = "$context.protocol"
      sourceIp         = "$context.identity.sourceIp"
      integrationError = "$context.integrationErrorMessage"
    })
  }

  tags = local.common_tags
}

# API Gateway access logs (request metadata only — never request bodies).
resource "aws_cloudwatch_log_group" "api_gw" {
  name              = "/aws/apigateway/${local.name_prefix}-api"
  retention_in_days = 14
  tags              = local.common_tags
}

resource "aws_iam_role" "api_gw_logs" {
  name = "${local.name_prefix}-apigw-logs-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "apigateway.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = local.common_tags
}

resource "aws_iam_role_policy" "api_gw_logs" {
  name = "${local.name_prefix}-apigw-logs-policy"
  role = aws_iam_role.api_gw_logs.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = ["${aws_cloudwatch_log_group.api_gw.arn}:*"]
    }]
  })
}

# Account-level CloudWatch role for API Gateway logging (one per account).
resource "aws_api_gateway_account" "main" {
  cloudwatch_role_arn = aws_iam_role.api_gw_logs.arn
}

# --- CloudWatch alarms ---

resource "aws_cloudwatch_metric_alarm" "ingest_errors" {
  alarm_name          = "${local.name_prefix}-ingest-errors"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "Errors"
  namespace           = "AWS/Lambda"
  period              = 300
  statistic           = "Sum"
  threshold           = 0
  alarm_description   = "Ingest Lambda errors"
  dimensions = {
    FunctionName = aws_lambda_function.main["ingest"].function_name
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "query_errors" {
  alarm_name          = "${local.name_prefix}-query-errors"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "Errors"
  namespace           = "AWS/Lambda"
  period              = 300
  statistic           = "Sum"
  threshold           = 0
  alarm_description   = "Query Lambda errors"
  dimensions = {
    FunctionName = aws_lambda_function.main["query"].function_name
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "api_4xx" {
  alarm_name          = "${local.name_prefix}-api-4xx"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "4XXError"
  namespace           = "AWS/ApiGateway"
  period              = 300
  statistic           = "Sum"
  threshold           = 100
  alarm_description   = "API Gateway 4xx rate elevated"
  dimensions = {
    ApiName = aws_api_gateway_rest_api.main.name
    Stage   = aws_api_gateway_stage.main.stage_name
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "api_5xx" {
  alarm_name          = "${local.name_prefix}-api-5xx"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "5XXError"
  namespace           = "AWS/ApiGateway"
  period              = 300
  statistic           = "Sum"
  threshold           = 0
  alarm_description   = "API Gateway 5xx errors"
  dimensions = {
    ApiName = aws_api_gateway_rest_api.main.name
    Stage   = aws_api_gateway_stage.main.stage_name
  }
  tags = local.common_tags
}

# Usage plan with per-key throttle/burst + daily quota (abuse prevention).
# SDK-facing methods do NOT require an API key (stock Sentry SDKs never send
# one); the usage plan applies to clients that present the issued API key.
# SDK traffic rate limiting is enforced in the ingest handler (429 +
# Retry-After). The THROTTLED/QUOTA_EXCEEDED gateway responses above guarantee
# gateway-level limits also surface per the response contract.

resource "aws_api_gateway_api_key" "main" {
  name = "${local.name_prefix}-key"
  tags = local.common_tags
}

resource "aws_api_gateway_usage_plan" "main" {
  name = "${local.name_prefix}-usage-plan"
  tags = local.common_tags

  api_stages {
    api_id = aws_api_gateway_rest_api.main.id
    stage  = aws_api_gateway_stage.main.stage_name
  }

  throttle_settings {
    burst_limit = var.usage_plan_burst_limit
    rate_limit  = var.usage_plan_rate_limit
  }

  quota_settings {
    limit  = var.usage_plan_quota
    period = "DAY"
  }
}

resource "aws_api_gateway_usage_plan_key" "main" {
  key_id        = aws_api_gateway_api_key.main.id
  key_type      = "API_KEY"
  usage_plan_id = aws_api_gateway_usage_plan.main.id
}
