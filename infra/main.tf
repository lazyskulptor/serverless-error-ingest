# ---------------------------------------------------------------------------
# Serverless Sentry-Compatible Error Ingest — OpenTofu infrastructure
# API Gateway (REST) + Lambda (ingest) + S3 (raw) + DynamoDB (index)
# ---------------------------------------------------------------------------

terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 4.0"
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

provider "cloudflare" {}

locals {
  name_prefix           = var.name_prefix
  custom_domain_enabled = var.domain_name != ""
  alarm_actions         = var.alarm_sns_topic_arn == "" ? [] : [var.alarm_sns_topic_arn]

  common_tags = {
    Project     = "serverless-sentry-ingest"
    ManagedBy   = "opentofu"
    Environment = var.environment
  }

  route_resource_ids = {
    envelope = aws_api_gateway_resource.envelope.id
    store    = aws_api_gateway_resource.store.id
  }

  api_routes = {
    envelope = { resource = "envelope", lambda = "ingest", http = "POST" }
    store    = { resource = "store", lambda = "ingest", http = "POST" }
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

resource "aws_iam_role_policy_attachment" "ingest_logs" {
  role       = aws_iam_role.ingest.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
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
        EVENT_TTL_DAYS = tostring(var.event_ttl_days)
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

# --- Lambda proxy methods (POST only) ---

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

# --- Deployment and stage ---

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

# --- Optional custom domain and selectable DNS management ---

resource "aws_acm_certificate" "api" {
  count = local.custom_domain_enabled ? 1 : 0

  domain_name       = var.domain_name
  validation_method = "DNS"
  tags              = local.common_tags

  lifecycle {
    create_before_destroy = true
    precondition {
      condition = (
        (var.dns_provider == "aws" && var.route53_zone_id != "") ||
        (var.dns_provider == "cloudflare" && var.cloudflare_zone_id != "")
      )
      error_message = "A custom domain requires route53_zone_id for aws DNS or cloudflare_zone_id for cloudflare DNS."
    }
  }
}

locals {
  certificate_validation_options = local.custom_domain_enabled ? {
    for option in aws_acm_certificate.api[0].domain_validation_options : option.domain_name => {
      name  = option.resource_record_name
      type  = option.resource_record_type
      value = option.resource_record_value
    }
  } : {}
}

resource "aws_route53_record" "certificate_validation" {
  for_each = local.custom_domain_enabled && var.dns_provider == "aws" ? local.certificate_validation_options : {}

  zone_id         = var.route53_zone_id
  name            = each.value.name
  type            = each.value.type
  ttl             = 300
  records         = [each.value.value]
  allow_overwrite = true
}

resource "cloudflare_record" "certificate_validation" {
  for_each = local.custom_domain_enabled && var.dns_provider == "cloudflare" ? local.certificate_validation_options : {}

  zone_id         = var.cloudflare_zone_id
  name            = trimsuffix(each.value.name, ".")
  type            = each.value.type
  content         = trimsuffix(each.value.value, ".")
  ttl             = 300
  proxied         = false
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "api" {
  count = local.custom_domain_enabled ? 1 : 0

  certificate_arn = aws_acm_certificate.api[0].arn
  validation_record_fqdns = var.dns_provider == "aws" ? [
    for record in aws_route53_record.certificate_validation : record.fqdn
    ] : [
    for record in cloudflare_record.certificate_validation : record.hostname
  ]
}

resource "aws_api_gateway_domain_name" "api" {
  count = local.custom_domain_enabled ? 1 : 0

  domain_name              = var.domain_name
  regional_certificate_arn = aws_acm_certificate_validation.api[0].certificate_arn

  endpoint_configuration {
    types = ["REGIONAL"]
  }

  tags = local.common_tags
}

resource "aws_api_gateway_base_path_mapping" "api" {
  count = local.custom_domain_enabled ? 1 : 0

  api_id      = aws_api_gateway_rest_api.main.id
  stage_name  = aws_api_gateway_stage.main.stage_name
  domain_name = aws_api_gateway_domain_name.api[0].domain_name
}

resource "aws_route53_record" "api" {
  count = local.custom_domain_enabled && var.dns_provider == "aws" ? 1 : 0

  zone_id = var.route53_zone_id
  name    = var.domain_name
  type    = "A"

  alias {
    name                   = aws_api_gateway_domain_name.api[0].regional_domain_name
    zone_id                = aws_api_gateway_domain_name.api[0].regional_zone_id
    evaluate_target_health = false
  }
}

resource "cloudflare_record" "api" {
  count = local.custom_domain_enabled && var.dns_provider == "cloudflare" ? 1 : 0

  zone_id = var.cloudflare_zone_id
  name    = var.domain_name
  type    = "CNAME"
  content = aws_api_gateway_domain_name.api[0].regional_domain_name
  ttl     = var.cloudflare_proxied ? 1 : 300
  proxied = var.cloudflare_proxied
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
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions
  dimensions = {
    FunctionName = aws_lambda_function.main["ingest"].function_name
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
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions
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
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions
  dimensions = {
    ApiName = aws_api_gateway_rest_api.main.name
    Stage   = aws_api_gateway_stage.main.stage_name
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "s3_object_count" {
  alarm_name          = "${local.name_prefix}-s3-object-count"
  comparison_operator = "GreaterThanOrEqualToThreshold"
  evaluation_periods  = 1
  metric_name         = "NumberOfObjects"
  namespace           = "AWS/S3"
  period              = 86400
  statistic           = "Average"
  threshold           = var.s3_object_count_alarm_threshold
  alarm_description   = "Raw archive object count reached the configured growth threshold; S3 storage metrics update daily"
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions
  treat_missing_data  = "notBreaching"
  dimensions = {
    BucketName  = aws_s3_bucket.raw.id
    StorageType = "AllStorageTypes"
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "s3_bucket_size" {
  alarm_name          = "${local.name_prefix}-s3-bucket-size"
  comparison_operator = "GreaterThanOrEqualToThreshold"
  evaluation_periods  = 1
  metric_name         = "BucketSizeBytes"
  namespace           = "AWS/S3"
  period              = 86400
  statistic           = "Average"
  threshold           = var.s3_bucket_size_alarm_bytes
  alarm_description   = "Raw archive size reached the configured growth threshold; S3 storage metrics update daily"
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions
  treat_missing_data  = "notBreaching"
  dimensions = {
    BucketName  = aws_s3_bucket.raw.id
    StorageType = "StandardStorage"
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "api_request_volume" {
  alarm_name          = "${local.name_prefix}-api-hourly-requests"
  comparison_operator = "GreaterThanOrEqualToThreshold"
  evaluation_periods  = 1
  metric_name         = "Count"
  namespace           = "AWS/ApiGateway"
  period              = 3600
  statistic           = "Sum"
  threshold           = var.api_hourly_request_alarm_threshold
  alarm_description   = "API request volume reached the configured hourly abuse threshold"
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions
  treat_missing_data  = "notBreaching"
  dimensions = {
    ApiName = aws_api_gateway_rest_api.main.name
    Stage   = aws_api_gateway_stage.main.stage_name
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "ingest_throttles" {
  alarm_name          = "${local.name_prefix}-ingest-throttles"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "Throttles"
  namespace           = "AWS/Lambda"
  period              = 300
  statistic           = "Sum"
  threshold           = 0
  alarm_description   = "Ingest Lambda was throttled"
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions
  treat_missing_data  = "notBreaching"
  dimensions = {
    FunctionName = aws_lambda_function.main["ingest"].function_name
  }
  tags = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "dynamodb_write_throttles" {
  for_each = {
    projects = aws_dynamodb_table.projects.name
    events   = aws_dynamodb_table.events.name
  }

  alarm_name          = "${local.name_prefix}-${each.key}-write-throttles"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "WriteThrottleEvents"
  namespace           = "AWS/DynamoDB"
  period              = 300
  statistic           = "Sum"
  threshold           = 0
  alarm_description   = "DynamoDB ${each.key} writes were throttled"
  alarm_actions       = local.alarm_actions
  ok_actions          = local.alarm_actions
  treat_missing_data  = "notBreaching"
  dimensions = {
    TableName = each.value
  }
  tags = local.common_tags
}
