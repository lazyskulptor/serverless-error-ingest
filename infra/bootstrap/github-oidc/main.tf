terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

data "aws_caller_identity" "current" {}

resource "aws_iam_openid_connect_provider" "github" {
  url             = "https://token.actions.githubusercontent.com"
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1"]
}

data "aws_iam_policy_document" "plan_trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${var.github_repository}:pull_request"]
    }
  }
}

data "aws_iam_policy_document" "apply_trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${var.github_repository}:environment:${var.github_environment}"]
    }
  }
}

resource "aws_iam_role" "plan" {
  name               = "${var.resource_name_prefix}-github-plan"
  assume_role_policy = data.aws_iam_policy_document.plan_trust.json
}

resource "aws_iam_role" "apply" {
  name               = "${var.resource_name_prefix}-github-apply"
  assume_role_policy = data.aws_iam_policy_document.apply_trust.json
}

data "aws_iam_policy_document" "state" {
  statement {
    actions   = ["s3:ListBucket", "s3:GetBucketVersioning"]
    resources = ["arn:aws:s3:::${var.state_bucket}"]
  }
  statement {
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["arn:aws:s3:::${var.state_bucket}/${var.state_key}"]
  }
  statement {
    actions = [
      "dynamodb:DescribeTable",
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:DeleteItem"
    ]
    resources = ["arn:aws:dynamodb:${var.aws_region}:${data.aws_caller_identity.current.account_id}:table/${var.state_lock_table}"]
  }
}

data "aws_iam_policy_document" "runtime_read" {
  statement {
    actions = [
      "acm:DescribeCertificate", "acm:ListCertificates", "apigateway:GET",
      "cloudwatch:DescribeAlarms", "dynamodb:DescribeTable", "dynamodb:ListTagsOfResource",
      "iam:GetRole", "iam:GetRolePolicy", "iam:ListAttachedRolePolicies", "iam:ListRolePolicies",
      "lambda:GetFunction", "lambda:GetPolicy", "logs:DescribeLogGroups", "s3:GetBucket*",
      "s3:ListBucket", "wafv2:GetWebACL", "wafv2:GetWebACLForResource", "wafv2:ListTagsForResource",
      "route53:GetHostedZone", "route53:ListResourceRecordSets"
    ]
    resources = ["*"]
  }
}

data "aws_iam_policy_document" "runtime_apply" {
  statement {
    actions = [
      "acm:*", "apigateway:*", "cloudwatch:DeleteAlarms", "cloudwatch:PutMetricAlarm",
      "dynamodb:CreateTable", "dynamodb:DeleteTable", "dynamodb:DescribeTable", "dynamodb:GetItem",
      "dynamodb:ListTagsOfResource", "dynamodb:PutItem", "dynamodb:TagResource", "dynamodb:UntagResource",
      "dynamodb:UpdateContinuousBackups", "dynamodb:UpdateTable", "dynamodb:UpdateTimeToLive",
      "iam:AttachRolePolicy", "iam:CreateRole", "iam:DeleteRole", "iam:DeleteRolePolicy", "iam:DetachRolePolicy",
      "iam:GetRole", "iam:GetRolePolicy", "iam:ListAttachedRolePolicies", "iam:ListRolePolicies", "iam:PassRole",
      "iam:PutRolePolicy", "iam:TagRole", "iam:UntagRole", "iam:UpdateAssumeRolePolicy",
      "lambda:AddPermission", "lambda:CreateFunction", "lambda:DeleteFunction", "lambda:GetFunction",
      "lambda:GetPolicy", "lambda:RemovePermission", "lambda:TagResource", "lambda:UntagResource",
      "lambda:UpdateFunctionCode", "lambda:UpdateFunctionConfiguration", "logs:*",
      "s3:CreateBucket", "s3:DeleteBucket", "s3:DeleteBucketPolicy", "s3:DeleteObject", "s3:GetBucket*",
      "s3:GetObject", "s3:HeadObject", "s3:ListBucket", "s3:PutBucket*", "s3:PutObject", "s3:TagResource",
      "wafv2:*"
    ]
    resources = ["*"]
  }
  dynamic "statement" {
    for_each = var.route53_zone_arn == "" ? [] : [var.route53_zone_arn]
    content {
      actions   = ["route53:ChangeResourceRecordSets", "route53:GetChange", "route53:GetHostedZone", "route53:ListResourceRecordSets"]
      resources = [statement.value]
    }
  }
}

resource "aws_iam_role_policy" "plan" {
  role   = aws_iam_role.plan.id
  policy = jsonencode({ Version = "2012-10-17", Statement = concat(jsondecode(data.aws_iam_policy_document.state.json).Statement, jsondecode(data.aws_iam_policy_document.runtime_read.json).Statement) })
}

resource "aws_iam_role_policy" "apply" {
  role   = aws_iam_role.apply.id
  policy = jsonencode({ Version = "2012-10-17", Statement = concat(jsondecode(data.aws_iam_policy_document.state.json).Statement, jsondecode(data.aws_iam_policy_document.runtime_apply.json).Statement) })
}
