terraform {
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  backend "gcs" {
    bucket = "gen-lang-client-0479899208-tfstate"
    prefix = "sre-bible"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}
