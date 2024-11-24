WIP based on the work of

Daniel Medeiros, Gabin Schieffer, Jacob Wahlgren, Ivy Peng. 2023. _A GPU-accelerated Molecular Docking Workflow with Kubernetes and Apache Airflow_. WOCC'23

Christopher Woods, University of Bristol, UK "Running Serverless HPC Workloads on Top of Kubernetes and Jupyter Notebooks" CNCF KubeCon18 



# AutoDock4 workflow on Apache Airflow
A workflow for molecular docking using AutoDock4. The workflow is implemented as a DAG, and can be run in Apache Airflow, on a Kubernetes cluster.

## Quickstart

### Folders
The main DAG is contained in `autodock.py`, we also provide with the following folders:
- `docker/` contains docker builds for images. each includes a `Dockerfile`, along with bash scripts which are included in the image;
- `iac/` contains files related to *infrastructure as code*, this includes ansible files for a quick deployment
- `rke2/` contains folders for each deployment with associated values and configurations 

### Usage


## Setup & Installation
### Requirements
- Ansible
- Ubuntu

## Relevant publications
- Daniel Medeiros, Gabin Schieffer, Jacob Wahlgren, Ivy Peng. 2023. _A GPU-accelerated Molecular Docking Workflow with Kubernetes and Apache Airflow_. WOCC'23

## FAQ
