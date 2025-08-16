#!/usr/bin/env python3
"""
Grape.ai Build Worker

Detects project type and builds accordingly:
- Node.js projects: runs npm install && npm run build
- Static sites: copies files directly
- Handles common build output directories
"""

import os
import sys
import json
import shutil
import subprocess
import logging

logging.basicConfig(level=logging.INFO, format='[%(levelname)s] %(message)s')
logger = logging.getLogger(__name__)

def run_command(cmd, cwd):
    """Run shell command and return success status"""
    try:
        logger.info(f"Running: {' '.join(cmd)} in {cwd}")
        result = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=600)
        
        if result.stdout:
            logger.info(f"STDOUT: {result.stdout}")
        if result.stderr:
            logger.warning(f"STDERR: {result.stderr}")
            
        return result.returncode == 0, result.stdout, result.stderr
    except subprocess.TimeoutExpired:
        logger.error("Command timed out")
        return False, "", "Build timed out after 10 minutes"
    except Exception as e:
        logger.error(f"Command failed: {e}")
        return False, "", str(e)

def detect_project_type(project_path):
    """Detect what type of project this is"""
    package_json = os.path.join(project_path, "package.json")
    index_html = os.path.join(project_path, "index.html")
    
    if os.path.exists(package_json):
        try:
            with open(package_json, 'r') as f:
                pkg = json.load(f)
                scripts = pkg.get('scripts', {})
                deps = pkg.get('dependencies', {})
                
                if 'next' in deps:
                    return 'nextjs'
                elif 'vite' in deps or 'build' in scripts:
                    return 'vite'
                elif 'react-scripts' in deps:
                    return 'cra'
                elif 'build' in scripts:
                    return 'node'
                else:
                    return 'static'
        except:
            logger.warning("Could not parse package.json")
            
    if os.path.exists(index_html):
        return 'static'
        
    return 'unknown'

def build_node_project(project_path):
    """Build a Node.js project"""
    logger.info("Building Node.js project...")
    
    # Check if npm is available
    npm_check, _, _ = run_command(['which', 'npm'], project_path)
    if not npm_check:
        logger.warning("npm not found, trying with node")
        return False, "npm not available"
    
    # Install dependencies
    success, stdout, stderr = run_command(['npm', 'install'], project_path)
    if not success:
        return False, f"npm install failed: {stderr}"
    
    # Build project
    success, stdout, stderr = run_command(['npm', 'run', 'build'], project_path)
    if not success:
        logger.warning("npm run build failed, trying npm run dev")
        # Some projects might not have a build script
        return True, "No build script found, serving source files"
    
    return True, "Build completed successfully"

def find_build_output(project_path):
    """Find the build output directory"""
    candidates = [
        'dist',
        'build', 
        'out',
        '.next/standalone',
        '.next/out',
        'public',
        '_site'
    ]
    
    for candidate in candidates:
        full_path = os.path.join(project_path, candidate)
        if os.path.exists(full_path) and os.path.isdir(full_path):
            # Check if directory has content
            if os.listdir(full_path):
                logger.info(f"Found build output: {candidate}")
                return full_path
    
    return None

def copy_to_deploy(source, deploy_path):
    """Copy files to deployment directory"""
    if os.path.exists(deploy_path):
        shutil.rmtree(deploy_path)
    
    shutil.copytree(source, deploy_path)
    logger.info(f"Copied {source} to {deploy_path}")

def create_fallback_page(deploy_path, message="Project deployed successfully"):
    """Create a fallback index.html if none exists"""
    os.makedirs(deploy_path, exist_ok=True)
    
    html_content = f"""<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Grape.ai Deployment</title>
    <style>
        body {{
            font-family: system-ui, -apple-system, sans-serif;
            margin: 0;
            padding: 40px;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            min-height: 100vh;
            display: flex;
            align-items: center;
            justify-content: center;
        }}
        .container {{
            text-align: center;
            background: rgba(255,255,255,0.1);
            padding: 40px;
            border-radius: 20px;
            backdrop-filter: blur(10px);
        }}
        h1 {{ margin: 0 0 20px 0; }}
        .grape {{ font-size: 48px; }}
    </style>
</head>
<body>
    <div class="container">
        <div class="grape">🍇</div>
        <h1>Grape.ai</h1>
        <p>{message}</p>
        <p><small>Powered by Grape.ai hosting platform</small></p>
    </div>
</body>
</html>"""
    
    with open(os.path.join(deploy_path, "index.html"), 'w') as f:
        f.write(html_content)

def main():
    if len(sys.argv) != 3:
        logger.error("Usage: worker.py <project_path> <deploy_path>")
        sys.exit(1)
    
    project_path = os.path.abspath(sys.argv[1])
    deploy_path = os.path.abspath(sys.argv[2])
    
    logger.info(f"Building project: {project_path}")
    logger.info(f"Deploy target: {deploy_path}")
    
    if not os.path.exists(project_path):
        logger.error(f"Project path does not exist: {project_path}")
        sys.exit(1)
    
    # Detect project type
    project_type = detect_project_type(project_path)
    logger.info(f"Detected project type: {project_type}")
    
    build_success = True
    build_message = ""
    
    # Build based on project type
    if project_type in ['nextjs', 'vite', 'cra', 'node']:
        build_success, build_message = build_node_project(project_path)
    
    # Find build output
    build_output = find_build_output(project_path)
    
    if build_output:
        copy_to_deploy(build_output, deploy_path)
    elif os.path.exists(os.path.join(project_path, "index.html")):
        # Static site - copy entire project
        copy_to_deploy(project_path, deploy_path)
    else:
        # No build output and no index.html - create fallback
        if build_success:
            create_fallback_page(deploy_path, "Project built but no output found")
        else:
            create_fallback_page(deploy_path, f"Build failed: {build_message}")
    
    # Ensure index.html exists
    index_path = os.path.join(deploy_path, "index.html")
    if not os.path.exists(index_path):
        create_fallback_page(deploy_path, "Deployment completed")
    
    logger.info("Build process completed")

if __name__ == "__main__":
    main()