This project is an agentic workflow that uploads events to Google Calendar via Google Cloud Platform service.

Since this process requires handling sensitive data, this GitHub repository is fully stripped of sensitive data and cannot, on its own, connect to any specific Google Calendar.

### How to start the agent
This program does not have a GUI. Its user interface is exclusively within the terminal. Enter the agent's root directory, then enter `go run .`, which will run all Go files of the directory simultaneously.

### Basic installations
This workflow requires Ollama, Redis, and MongoDB, which will serve LLM responses, short-term memory, and persistent storage, respectively.

Install Ollama and run Llama 3 (or Llama 3.2) on it, so that this program can connect to a locally-hosted LLM.

To install and start MongoDB, enter `brew tap mongodb/brew`, then `brew install mongodb-community`, then `brew services start mongodb-community`.

To install and start Redis, enter `brew install redis`, then `brew services start redis`.

### How to connect the agent to Google Calendar
Enter your Google Cloud account and generate a service account by entering "IAM & Admin" -> "Service Accounts" -> "Create new service account".

After you created a service account, enter this specific service account. To get your private credentials file, click on "Add key" -> "Create new key" -> Choose JSON and click Create. Drag the downloaded file into this agent's root directory.

### How to connect the agent to the proper email address
The agent is configured to accept an environment variable as the Google Calendar email-address to route to. Set this environment variable to your own email-address by entering `export CALENDAR_EMAIL="your.email@gmail.com"` into the terminal.