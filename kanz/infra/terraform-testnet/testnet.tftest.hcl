mock_provider "aws" {
  mock_data "aws_availability_zones" {
    defaults = {
      names = ["ap-northeast-1a"]
    }
  }

  mock_data "aws_ssm_parameter" {
    defaults = {
      value = "ami-testnet-x86"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/kanz-test-role"
    }
  }
}

run "cost_and_security_envelope" {
  command = apply

  variables {
    budget_email = "alerts@example.invalid"
  }

  assert {
    condition     = aws_instance.node.instance_type == "t3a.large"
    error_message = "The testnet instance escaped the reviewed t3a.large cost envelope."
  }

  assert {
    condition     = aws_instance.node.credit_specification[0].cpu_credits == "standard"
    error_message = "T3 unlimited credits can create unbounded surplus-credit charges."
  }

  assert {
    condition     = aws_instance.node.metadata_options[0].http_tokens == "required"
    error_message = "IMDSv2 must be mandatory."
  }

  assert {
    condition     = aws_instance.node.metadata_options[0].http_put_response_hop_limit == 1
    error_message = "The node metadata hop limit must remain 1 so ordinary pods cannot obtain the instance role."
  }

  assert {
    condition     = aws_kms_key.vault_unseal.enable_key_rotation
    error_message = "Vault auto-unseal must use a rotation-enabled customer-managed KMS key."
  }

  assert {
    condition = toset(jsondecode(aws_iam_role_policy.vault_unseal.policy).Statement[0].Action) == toset([
      "kms:Encrypt",
      "kms:Decrypt",
      "kms:DescribeKey",
    ])
    error_message = "The node role must receive only the three operations required for Vault auto-unseal."
  }

  assert {
    condition     = length(aws_ecr_repository.capital_path) == 11 && alltrue([for repository in aws_ecr_repository.capital_path : repository.image_tag_mutability == "IMMUTABLE"])
    error_message = "Every and only capital-path image repository must reject mutable tags."
  }

  assert {
    condition     = jsondecode(aws_iam_role.github_ecr_publish.assume_role_policy).Statement[0].Condition.StringEquals["token.actions.githubusercontent.com:sub"] == "repo:eighred/kanz:ref:refs/heads/main"
    error_message = "The AWS publisher identity must be assumable only by this repository's main branch."
  }

  assert {
    condition = toset(jsondecode(aws_iam_role_policy.node_ecr_pull.policy).Statement[1].Action) == toset([
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
    ])
    error_message = "The node role must remain pull-only for capital-path ECR repositories."
  }

  assert {
    condition     = aws_instance.node.root_block_device[0].encrypted && aws_ebs_volume.data.encrypted
    error_message = "Both OS and durable data volumes must be encrypted."
  }

  assert {
    condition     = aws_instance.node.root_block_device[0].volume_size + aws_ebs_volume.data.size == 100
    error_message = "The reviewed EBS envelope is exactly 100 GiB."
  }

  assert {
    condition     = aws_budgets_budget.monthly.limit_amount == "100"
    error_message = "The mandatory monthly budget must remain USD 100."
  }

  assert {
    condition     = aws_instance.node.disable_api_termination
    error_message = "Accidental instance termination must remain API-protected."
  }

  assert {
    condition = (
      strcontains(aws_instance.node.user_data, "for _ in $(seq 1 300)") &&
      !strcontains(aws_instance.node.user_data, "$$(seq 1 300)")
    )
    error_message = "Terraform must render valid Bash command substitutions in user data."
  }

  assert {
    condition     = strcontains(aws_instance.node.user_data, "kubectl get nodes -o name")
    error_message = "Bootstrap must wait for node registration before asserting readiness."
  }

  assert {
    condition = (
      strcontains(aws_instance.node.user_data, "7656c21bcc13700566830f6bc4d753063513e6f7") &&
      strcontains(aws_instance.node.user_data, "credentialprovider.kubelet.k8s.io/v1") &&
      strcontains(aws_instance.node.user_data, "/var/lib/rancher/credentialprovider/bin/ecr-credential-provider")
    )
    error_message = "Cold nodes must install the pinned kubelet ECR credential provider before k3s starts."
  }
}

run "rejects_non_tokyo_region" {
  command = plan

  variables {
    region       = "us-east-1"
    budget_email = "alerts@example.invalid"
  }

  expect_failures = [var.region]
}

run "rejects_cost_expansion" {
  command = plan

  variables {
    instance_type = "m6i.xlarge"
    budget_email  = "alerts@example.invalid"
  }

  expect_failures = [var.instance_type]
}
