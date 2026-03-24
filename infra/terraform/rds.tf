# ==============================================================================
# RDS Subnet Group
# ==============================================================================

resource "aws_db_subnet_group" "main" {
  name       = "${var.project_name}-db-subnet-group"
  subnet_ids = aws_subnet.private[*].id

  tags = {
    Name = "${var.project_name}-db-subnet-group"
  }
}

# ==============================================================================
# RDS PostgreSQL Instance
# ==============================================================================

resource "aws_db_instance" "postgres" {
  identifier = "${var.project_name}-db"

  # Engine
  engine         = "postgres"
  engine_version = "16.4"

  # Instance
  instance_class    = "db.t3.micro" # Free tier eligible
  allocated_storage = 20
  storage_type      = "gp2"

  # Database
  db_name  = "livecart"
  username = var.db_username
  password = var.db_password
  port     = 5432

  # Network
  vpc_security_group_ids = [aws_security_group.rds.id]
  db_subnet_group_name   = aws_db_subnet_group.main.name
  publicly_accessible    = false

  # Backup & Maintenance
  backup_retention_period = 7
  backup_window           = "03:00-04:00"
  maintenance_window      = "Mon:04:00-Mon:05:00"

  # Performance Insights (free tier)
  performance_insights_enabled = true

  # Staging optimizations
  skip_final_snapshot       = true
  delete_automated_backups  = true
  apply_immediately         = true
  auto_minor_version_upgrade = true

  tags = {
    Name = "${var.project_name}-db"
  }
}
