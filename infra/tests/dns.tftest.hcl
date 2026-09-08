mock_provider "aws" {
  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/mock-role"
    }
  }
  mock_resource "aws_api_gateway_rest_api" {
    defaults = {
      execution_arn = "arn:aws:execute-api:ap-northeast-2:123456789012:mock-api"
    }
  }
  mock_resource "aws_lambda_function" {
    defaults = {
      arn = "arn:aws:lambda:ap-northeast-2:123456789012:function:mock"
    }
  }
  mock_resource "aws_cloudwatch_log_group" {
    defaults = {
      arn = "arn:aws:logs:ap-northeast-2:123456789012:log-group:mock"
    }
  }
  mock_resource "aws_api_gateway_stage" {
    defaults = {
      arn = "arn:aws:apigateway:ap-northeast-2::/restapis/mock/stages/v1"
    }
  }
  mock_resource "aws_wafv2_web_acl" {
    defaults = {
      arn = "arn:aws:wafv2:ap-northeast-2:123456789012:regional/webacl/mock/00000000-0000-0000-0000-000000000000"
    }
  }
}
mock_provider "cloudflare" {}

variables {
  raw_bucket_name = "example-error-ingest-test"
}

run "direct_api_without_custom_domain" {
  command = plan

  assert {
    condition     = length(aws_acm_certificate.api) == 0
    error_message = "Direct API mode must not create an ACM certificate."
  }

  assert {
    condition     = length(aws_route53_record.api) == 0 && length(cloudflare_record.api) == 0
    error_message = "Direct API mode must not create DNS records."
  }

  assert {
    condition     = keys(aws_api_gateway_method.main) == ["envelope", "store"]
    error_message = "Only POST ingest methods may be deployed."
  }
}

run "aws_dns_branch" {
  command = plan

  variables {
    domain_name     = "errors.example.com"
    dns_provider    = "aws"
    route53_zone_id = "Z1234567890"
  }

  assert {
    condition     = length(aws_route53_record.api) == 1 && length(cloudflare_record.api) == 0
    error_message = "AWS mode must create only the Route 53 API record."
  }
}

run "cloudflare_dns_branch" {
  command = plan

  variables {
    domain_name        = "errors.example.com"
    dns_provider       = "cloudflare"
    cloudflare_zone_id = "0123456789abcdef0123456789abcdef"
  }

  assert {
    condition     = length(aws_route53_record.api) == 0 && length(cloudflare_record.api) == 1
    error_message = "Cloudflare mode must create only the Cloudflare API record."
  }

  assert {
    condition     = cloudflare_record.api[0].proxied == false
    error_message = "Cloudflare API records must default to DNS-only."
  }
}
