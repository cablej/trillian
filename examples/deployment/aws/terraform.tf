variable "WHITELIST_CIDR" {
  description="Your IP block to whitelist access from"
}
variable "MYSQL_ROOT_PASSWORD" { }

provider "aws" {
  region     = "us-west-2"
}

/* The Database */

resource "aws_rds_cluster" "trillian" {
  cluster_identifier      = "trillian"
  database_name           = "test"
  engine = "aurora-mysql"
  engine_version          = "5.7.mysql_aurora.2.11.2"
  master_username         = "root"
  master_password         = var.MYSQL_ROOT_PASSWORD
  skip_final_snapshot     = true
  port                    = 3306
  vpc_security_group_ids  = ["${aws_security_group.trillian_db.id}"]
  availability_zones      = ["us-west-2a", "us-west-2b", "us-west-2c"]
  storage_encrypted       = true
  apply_immediately       = true

}

resource "aws_rds_cluster_instance" "cluster_instances" {
  count               = 1
  engine = "aurora-mysql"
  engine_version          = "5.7.mysql_aurora.2.11.2"
  identifier          = "trillian-${count.index}"
  cluster_identifier  = "${aws_rds_cluster.trillian.id}"
  instance_class      = "db.t2.medium"
  publicly_accessible = true
  apply_immediately   = true
}

resource "aws_security_group" "trillian_db" {
  name        = "trillian-db"
  description = "Allow MySQL from Trillian and Development CIDR"

  ingress {
    from_port   = 3306
    to_port     = 3306
    protocol    = "tcp"
    cidr_blocks = [var.WHITELIST_CIDR]
  }

  ingress {
    from_port   = 3306
    to_port     = 3306
    protocol    = "tcp"
    security_groups = ["${aws_security_group.trillian.id}"]
  }

  egress {
    from_port       = 0
    to_port         = 0
    protocol        = "-1"
    cidr_blocks     = ["0.0.0.0/0"]
  }
}

/*resource "aws_rds_cluster_parameter_group" "trillian" {
  name        = "trillian-pg"

  # Whether InnoDB returns errors rather than warnings for exceptional conditions.
  # replaces: `sql_mode = STRICT_ALL_TABLES`
  parameter {
    name  = "innodb_strict_mode"
    value = "1"
  }
}*/

/* The Instance */

resource "aws_security_group" "trillian" {
  name        = "trillian"
  description = "Expose Rest, TPC and SSH endpoint to local cidr"

  ingress {
    from_port   = 8090
    to_port     = 8091
    protocol    = "tcp"
    cidr_blocks = [var.WHITELIST_CIDR]
  }
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.WHITELIST_CIDR]
  }

  egress {
    from_port       = 0
    to_port         = 0
    protocol        = "-1"
    cidr_blocks     = ["0.0.0.0/0"]
  }
}

data "aws_ami" "amazon-linux-2" {
 most_recent = true
 owners = ["amazon"]

 filter {
   name   = "owner-alias"
   values = ["amazon"]
 }


 filter {
   name   = "name"
   values = ["amzn2-ami-hvm*"]
 }
}

resource "aws_instance" "trillian" {
  ami                         = data.aws_ami.amazon-linux-2.id
  instance_type               = "t2.medium"
  vpc_security_group_ids      = ["${aws_security_group.trillian.id}"]
  associate_public_ip_address = true
  key_name = "amazon-west"


  user_data =  <<EOF
#!/bin/bash

set -e

EOF

}
