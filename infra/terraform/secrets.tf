# ==============================================================================
# Secrets Manager - Clerk Credentials
# ==============================================================================

resource "aws_secretsmanager_secret" "clerk" {
  name                    = "${var.project_name}/${var.environment}/clerk"
  description             = "Clerk authentication credentials"
  recovery_window_in_days = 0 # Immediate deletion for staging

  tags = {
    Name = "${var.project_name}-clerk-secret"
  }
}

resource "aws_secretsmanager_secret_version" "clerk" {
  secret_id = aws_secretsmanager_secret.clerk.id
  secret_string = jsonencode({
    CLERK_SECRET_KEY     = var.clerk_secret_key
    CLERK_FRONTEND_API   = var.clerk_frontend_api
    CLERK_WEBHOOK_SECRET = var.clerk_webhook_secret
  })
}

# ==============================================================================
# Secrets Manager - Database Credentials
# ==============================================================================

resource "aws_secretsmanager_secret" "database" {
  name                    = "${var.project_name}/${var.environment}/database"
  description             = "Database credentials"
  recovery_window_in_days = 0 # Immediate deletion for staging

  tags = {
    Name = "${var.project_name}-database-secret"
  }
}

resource "aws_secretsmanager_secret_version" "database" {
  secret_id = aws_secretsmanager_secret.database.id
  secret_string = jsonencode({
    DATABASE_URL = "postgres://${var.db_username}:${var.db_password}@${aws_db_instance.postgres.endpoint}/${aws_db_instance.postgres.db_name}?sslmode=require"
  })
}
