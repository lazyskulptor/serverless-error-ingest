# GitHub OIDC bootstrap

This standalone stack creates the GitHub OIDC provider and Release apply role.
Run it once with trusted local administrator credentials; do not place it in
the runtime backend it authorizes.

Copy values into a local `.tfvars` file outside source control, then run
`tofu init`, `tofu plan`, and `tofu apply`. If the account already has the
GitHub provider, import it before applying:

`tofu import aws_iam_openid_connect_provider.github arn:aws:iam::<account>:oidc-provider/token.actions.githubusercontent.com`

Review generated IAM policies before applying. The role trusts only the
configured GitHub environment. Remove
repository environment access first during break-glass revocation, then remove
the role or its trust policy.

Some GitHub repositories emit immutable owner/repository IDs in the OIDC `sub`
claim. Inspect a denied CloudTrail `AssumeRoleWithWebIdentity` event and set
`github_repository` to the exact claim form when it differs from `owner/name`;
never broaden trust to all repositories as a workaround. Permit the Release tag
pattern in the GitHub environment deployment policy as well as any intentional
manual-dispatch branch.
