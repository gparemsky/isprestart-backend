import requests

url = "http://localhost:8081/hello"
response = requests.get(url)

print(response.json())